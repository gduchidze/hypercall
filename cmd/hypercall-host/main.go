package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/gduchidze/hypercall/internal/sandbox/hostdoctor"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 || os.Args[1] != "doctor" {
		fmt.Fprintln(os.Stderr, "usage: hypercall-host doctor [--json] [--config artifacts.json] [--cgroup-dir delegated-directory]")
		return 2
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "write a structured host prerequisite report")
	config := flags.String("config", "", "path to explicit runtime artifact configuration JSON")
	cgroups := flags.String("cgroup-dir", "/sys/fs/cgroup", "delegated cgroups v2 directory (creates and removes an empty probe child)")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "doctor does not accept positional arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report := hostdoctor.Run(ctx, hostdoctor.Options{ConfigPath: *config, CgroupDir: *cgroups})
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		fmt.Printf("Host prerequisites: %s/%s ready=%t\n", report.OS, report.Architecture, report.Ready)
		for _, check := range report.Checks {
			fmt.Printf("%s %s: %s\n", check.Status, check.ID, check.Detail)
		}
	}
	if !report.Ready {
		return 1
	}
	return 0
}
