// hypercall is the authenticated Go client for the public control plane.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	guest "github.com/gduchidze/hypercall/gen/go/hypercall/guest/v1"
	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out, errout io.Writer) error {
	flags := flag.NewFlagSet("hypercall", flag.ContinueOnError)
	flags.SetOutput(errout)
	endpoint := flags.String("endpoint", os.Getenv("HYPERCALL_ENDPOINT"), "gRPC host:port")
	project := flags.String("project", os.Getenv("HYPERCALL_PROJECT"), "project ID")
	plaintext := flags.Bool("plaintext", false, "allow plaintext on loopback for local development")
	caFile := flags.String("ca", "", "PEM CA bundle (default: system roots)")
	serverName := flags.String("server-name", "", "TLS server certificate name override")
	timeout := flags.Duration("timeout", 30*time.Second, "RPC deadline (also command deadline)")
	key := flags.String("key", "", "idempotency key required for submit and lifecycle actions")
	after := flags.Int64("after", 0, "exclusive durable event cursor")
	limit := flags.Int("limit", 100, "maximum page size")
	format := flags.String("format", "json", "report format: json or markdown")
	check := flags.Bool("check", false, "return failure when report CI decision fails")
	flags.Usage = func() {
		fmt.Fprintln(errout, "hypercall [flags] submit FILE | inspect KIND ID | list KIND | events/calls/steps BRANCH | suspend/resume/cancel BRANCH | fork CHECKPOINT MODEL_ID@DIGEST... | compare LEFT RIGHT COMPARATOR | exec BRANCH ARGV... | read-file BRANCH PATH | write-file BRANCH PATH LOCAL_FILE")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	argv := flags.Args()
	if len(argv) == 0 {
		flags.Usage()
		return errors.New("command required")
	}
	token := os.Getenv("HYPERCALL_API_KEY")
	if *endpoint == "" || *project == "" || token == "" {
		return errors.New("endpoint, project and HYPERCALL_API_KEY are required")
	}
	if *timeout <= 0 {
		return errors.New("positive timeout required")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: *serverName}
	if *caFile != "" {
		pem, e := os.ReadFile(*caFile)
		if e != nil {
			return e
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	var transport credentials.TransportCredentials = credentials.NewTLS(tlsConfig)
	if *plaintext {
		host, _, err := net.SplitHostPort(*endpoint)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("plaintext is limited to loopback endpoints")
		}
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(transport))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	client := pb.NewControlPlaneClient(conn)
	print := func(v proto.Message) error {
		b, e := (protojson.MarshalOptions{UseProtoNames: true, Indent: "  "}).Marshal(v)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintln(out, string(b))
		return e
	}
	need := func(n int) error {
		if len(argv) != n {
			return fmt.Errorf("%s expects %d arguments", argv[0], n-1)
		}
		return nil
	}
	switch argv[0] {
	case "compile-eval":
		if err := need(2); err != nil {
			return err
		}
		data, err := os.ReadFile(argv[1])
		if err != nil {
			return err
		}
		v, err := client.CompileEvaluation(ctx, &pb.CompileEvaluationRequest{ProjectId: *project, ConfigYaml: string(data)})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(v.PlanJson))
		return err
	case "evaluate":
		if err := need(3); err != nil {
			return err
		}
		if *key == "" {
			return errors.New("--key required")
		}
		data, err := os.ReadFile(argv[1])
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(argv[2])
		if err != nil {
			return err
		}
		var obs []json.RawMessage
		if err = json.Unmarshal(raw, &obs); err != nil {
			return err
		}
		request := &pb.SubmitEvaluationRequest{ProjectId: *project, ConfigYaml: string(data), IdempotencyKey: *key}
		for _, b := range obs {
			v := &pb.EvaluationObservation{}
			if err = protojson.Unmarshal(b, v); err != nil {
				return err
			}
			request.Observations = append(request.Observations, v)
		}
		v, err := client.SubmitEvaluation(ctx, request)
		if err != nil {
			return err
		}
		return print(v)
	case "evaluation":
		if err := need(2); err != nil {
			return err
		}
		v, err := client.GetEvaluation(ctx, &pb.GetEvaluationRequest{ProjectId: *project, Id: argv[1]})
		if err != nil {
			return err
		}
		return print(v)
	case "report":
		if err := need(2); err != nil {
			return err
		}
		if *format != "json" && *format != "markdown" {
			return errors.New("--format must be json or markdown")
		}
		v, err := client.GetReport(ctx, &pb.GetReportRequest{ProjectId: *project, Id: argv[1]})
		if err != nil {
			return err
		}
		data := string(v.ReportJson)
		if *format == "markdown" {
			data = v.Markdown
		}
		if _, err = fmt.Fprintln(out, data); err != nil {
			return err
		}
		if *check && !v.CiPass {
			return fmt.Errorf("evaluation CI failed: %s", strings.Join(v.Reasons, ","))
		}
		return nil
	case "analytics-status":
		if err := need(1); err != nil {
			return err
		}
		v, err := client.ProjectionStatus(ctx, &pb.ProjectionStatusRequest{ProjectId: *project})
		if err != nil {
			return err
		}
		return print(v)
	case "submit":
		if err := need(2); err != nil {
			return err
		}
		if *key == "" {
			return errors.New("--key required; reuse it to reconcile an ambiguous submission")
		}
		data, err := os.ReadFile(argv[1])
		if err != nil {
			return err
		}
		v, err := client.SubmitManifest(ctx, &pb.SubmitManifestRequest{ProjectId: *project, ManifestYaml: string(data), IdempotencyKey: *key})
		if err != nil {
			return err
		}
		return print(v)
	case "inspect":
		if err := need(3); err != nil {
			return err
		}
		v, e := client.GetResource(ctx, &pb.GetResourceRequest{ProjectId: *project, Kind: argv[1], Id: argv[2]})
		if e != nil {
			return e
		}
		return print(v)
	case "list":
		if err := need(2); err != nil {
			return err
		}
		v, e := client.ListResources(ctx, &pb.ListResourcesRequest{ProjectId: *project, Kind: argv[1], Limit: int32(*limit)})
		if e != nil {
			return e
		}
		return print(v)
	case "events":
		if err := need(2); err != nil {
			return err
		}
		v, e := client.Events(ctx, &pb.EventsRequest{ProjectId: *project, BranchId: argv[1], AfterSequence: *after, Limit: int32(*limit)})
		if e != nil {
			return e
		}
		return print(v)
	case "calls":
		if err := need(2); err != nil {
			return err
		}
		v, e := client.Calls(ctx, &pb.CallsRequest{ProjectId: *project, BranchId: argv[1], AfterSequence: *after, Limit: int32(*limit)})
		if e != nil {
			return e
		}
		return print(v)
	case "steps":
		if err := need(2); err != nil {
			return err
		}
		v, e := client.Steps(ctx, &pb.StepsRequest{ProjectId: *project, BranchId: argv[1], AfterStep: *after, Limit: int32(*limit)})
		if e != nil {
			return e
		}
		return print(v)
	case "fork":
		if len(argv) < 3 || len(argv) > 4 || *key == "" {
			return errors.New("fork expects CHECKPOINT and 1..2 MODEL_ID@DIGEST candidates, with --key")
		}
		models := []*pb.Ref{}
		for _, value := range argv[2:] {
			id, digest, ok := strings.Cut(value, "@")
			if !ok || id == "" || !strings.HasPrefix(digest, "sha256:") {
				return errors.New("candidate must be immutable MODEL_ID@sha256:DIGEST")
			}
			models = append(models, &pb.Ref{Id: id, Digest: digest})
		}
		v, e := client.Fork(ctx, &pb.ForkRequest{ProjectId: *project, CheckpointId: argv[1], Models: models, IdempotencyKey: *key})
		if e != nil {
			return e
		}
		return print(v)
	case "compare":
		if err := need(4); err != nil {
			return err
		}
		v, e := client.Compare(ctx, &pb.CompareRequest{ProjectId: *project, LeftBranch: argv[1], RightBranch: argv[2], Comparator: argv[3]})
		if e != nil {
			return e
		}
		return print(v)
	case "suspend", "resume", "cancel":
		if err := need(2); err != nil {
			return err
		}
		if *key == "" {
			return errors.New("--key required")
		}
		v, e := client.Action(ctx, &pb.ActionRequest{ProjectId: *project, BranchId: argv[1], Action: argv[0], IdempotencyKey: *key})
		if e != nil {
			return e
		}
		return print(v)
	case "exec":
		if len(argv) < 3 {
			return errors.New("exec expects BRANCH ARGV...")
		}
		millis := timeout.Milliseconds()
		if millis > int64(^uint32(0)) {
			return errors.New("exec timeout too large")
		}
		stream, e := client.Exec(ctx, &pb.ExecRequest{ProjectId: *project, BranchId: argv[1], Command: &guest.ExecRequest{Argv: argv[2:], TimeoutMs: uint32(millis)}})
		if e != nil {
			return e
		}
		for {
			event, e := stream.Recv()
			if e == io.EOF {
				return errors.New("command stream ended without result")
			}
			if e != nil {
				return e
			}
			switch v := event.Event.(type) {
			case *guest.ExecEvent_Stdout:
				if _, e = out.Write(v.Stdout); e != nil {
					return e
				}
			case *guest.ExecEvent_Stderr:
				if _, e = errout.Write(v.Stderr); e != nil {
					return e
				}
			case *guest.ExecEvent_Result:
				if v.Result.TimedOut {
					return errors.New("command timed out")
				}
				if v.Result.ExitCode != 0 {
					return fmt.Errorf("command exited %s", strconv.Itoa(int(v.Result.ExitCode)))
				}
				return nil
			}
		}
	case "read-file":
		if err := need(3); err != nil {
			return err
		}
		v, e := client.ReadFile(ctx, &pb.ReadFileRequest{ProjectId: *project, BranchId: argv[1], Path: argv[2]})
		if e != nil {
			return e
		}
		_, e = out.Write(v.Data)
		return e
	case "write-file":
		if err := need(4); err != nil {
			return err
		}
		f, e := os.Open(argv[3])
		if e != nil {
			return e
		}
		defer f.Close()
		data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if e != nil {
			return e
		}
		if len(data) > 1<<20 {
			return errors.New("file exceeds 1 MiB")
		}
		v, e := client.WriteFile(ctx, &pb.WriteFileRequest{ProjectId: *project, BranchId: argv[1], Path: argv[2], Data: data})
		if e != nil {
			return e
		}
		return print(v)
	default:
		return fmt.Errorf("unknown command %q", argv[0])
	}
}
