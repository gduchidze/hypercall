package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	pb "github.com/gduchidze/hypercall/gen/go/hypercall/v1"
	cp "github.com/gduchidze/hypercall/internal/controlplane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec map[string]any `yaml:"spec"`
}

func parseManifest(text string) ([]*pb.Resource, error) {
	if len(text) == 0 || len(text) > 1<<20 {
		return nil, status.Error(codes.InvalidArgument, "manifest must contain 1..1048576 bytes")
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	decoder.KnownFields(true)
	out := []*pb.Resource{}
	seen := map[string]bool{}
	tasks := 0
	for {
		var doc manifest
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid manifest: "+err.Error())
		}
		if len(out) >= 64 {
			return nil, status.Error(codes.InvalidArgument, "at most 64 manifest documents")
		}
		if doc.APIVersion != "hypercall.io/v1" || doc.Metadata.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "apiVersion hypercall.io/v1 and metadata.name required")
		}
		field := map[string]string{"Workspace": "workspace_spec", "ModelConfig": "model_config_spec", "Gateway": "gateway_spec", "Task": "task_spec"}[doc.Kind]
		if field == "" {
			return nil, status.Error(codes.InvalidArgument, "only definition kinds are accepted in manifests")
		}
		key := doc.Kind + "/" + doc.Metadata.Name
		if seen[key] {
			return nil, status.Error(codes.InvalidArgument, "duplicate definition "+key)
		}
		seen[key] = true
		if doc.Kind == "Task" {
			tasks++
		}
		if tasks > 0 && doc.Kind != "Task" {
			return nil, status.Error(codes.InvalidArgument, "Task must be the final document")
		}
		raw, e := json.Marshal(map[string]any{"kind": doc.Kind, "metadata": map[string]any{"name": doc.Metadata.Name}, field: doc.Spec})
		if e != nil {
			return nil, status.Error(codes.InvalidArgument, e.Error())
		}
		r := &pb.Resource{}
		if e = protojson.Unmarshal(raw, r); e != nil {
			return nil, status.Error(codes.InvalidArgument, e.Error())
		}
		if _, e = fromResource(r); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	if tasks != 1 {
		return nil, status.Error(codes.InvalidArgument, "manifest requires exactly one final Task")
	}
	return out, nil
}
func (s *Server) SubmitManifest(ctx context.Context, req *pb.SubmitManifestRequest) (*pb.Submission, error) {
	sc, e := scope(ctx, req.ProjectId)
	if e != nil {
		return nil, e
	}
	if req.IdempotencyKey == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key required")
	}
	docs, e := parseManifest(req.ManifestYaml)
	if e != nil {
		return nil, e
	}
	// Check every document's protocol shape before storing anything. Core semantic
	// validation happens during each definition's application. Named references resolve only
	// to immutable definitions in this manifest; external references need digests.
	refs := map[string]cp.Ref{}
	var taskID string
	for index, doc := range docs {
		if spec := doc.GetTaskSpec(); spec != nil {
			resolve := func(kind string, ref *pb.Ref) error {
				if ref == nil {
					return status.Error(codes.InvalidArgument, "missing "+kind+" reference")
				}
				if value, ok := refs[kind+"/"+ref.Id]; ok {
					if ref.Digest != "" && ref.Digest != value.Digest {
						return status.Error(codes.InvalidArgument, "manifest reference digest mismatch")
					}
					ref.Id = value.ID
					ref.Digest = value.Digest
				}
				return nil
			}
			if e = resolve("Workspace", spec.Workspace); e != nil {
				return nil, e
			}
			if e = resolve("Gateway", spec.Gateway); e != nil {
				return nil, e
			}
			for _, model := range spec.Models {
				if e = resolve("ModelConfig", model); e != nil {
					return nil, e
				}
			}
		}
		r, e := fromResource(doc)
		if e != nil {
			return nil, e
		}
		key := fmt.Sprintf("%s/definition/%d", req.IdempotencyKey, index)
		saved, e := s.service.PutResource(ctx, sc, r, key)
		if e != nil {
			return nil, canonical(e)
		}
		refs[doc.Kind+"/"+doc.Metadata.Name] = cp.Ref{ID: saved.Metadata.ID, Digest: saved.Metadata.Digest}
		if doc.Kind == "Task" {
			taskID = saved.Metadata.ID
		}
	}
	v, e := s.service.Submit(ctx, sc, taskID, req.IdempotencyKey+"/submit")
	if e != nil {
		return nil, canonical(e)
	}
	out, e := submission(v)
	return out, canonical(e)
}
