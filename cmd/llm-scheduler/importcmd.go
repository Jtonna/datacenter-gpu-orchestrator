package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/audit"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/bench"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgstore"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/cfgsvc"
	"github.com/Jtonna/datacenter-gpu-orchestrator/internal/config"
)

// importConfig is `llm-scheduler import-config`: it reads a YAML file in the
// old format, validates it, and writes it into an empty configuration
// database as revision 1 (audit source "import"). Benchmarks and suites in
// the bench block of the file are stored too.
func importConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bf := bindBootstrapDefaults(fs, false)
	replace := fs.Bool("replace", false, "replace a configuration the database already has (a new revision; the old one stays in the history)")
	check := fs.Bool("check", false, "only validate the file and print what would be imported")
	noModels := fs.Bool("no-model-check", false, "do not require the model files to exist")
	actor := fs.String("actor", "", "name recorded in the audit log (default: the OS user)")
	force := fs.Bool("force", false, "write even when the stored configuration has fields this version does not know (a newer version wrote them; they are dropped)")
	comment := fs.String("comment", "import of the YAML configuration file", "comment recorded in the audit log")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: llm-scheduler import-config [--data-dir DIR] [--models-dir DIR] [--replace] [--force] [--check] [--no-model-check] file.yaml")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	boot := bf.value()
	if boot.DataDir == "" {
		boot.DataDir = defaultDataDir
	}
	opts := config.Options{}
	if *noModels {
		opts.FileExists = func(string) bool { return true }
	}
	imp, err := config.ParseYAMLImport(data, boot, opts)
	if err != nil {
		fmt.Fprintln(stderr, "the file is not a valid configuration:")
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, n := range imp.Notes {
		fmt.Fprintln(stdout, "note:", n)
	}
	summary := fmt.Sprintf("%d profiles, %d tiers, %d benchmarks, %d suites", len(imp.Doc.Profiles), len(imp.Doc.Tiers), len(imp.Benchmarks), len(imp.Suites))
	if *check {
		fmt.Fprintln(stdout, "valid:", summary)
		return 0
	}
	st, err := bench.OpenStore(boot.DataDir)
	if err != nil {
		fmt.Fprintln(stderr, "cannot open the database in", boot.DataDir+":", err)
		return 1
	}
	defer st.Close()
	cs, err := cfgstore.Open(st)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	o := cfgsvc.WriteOpts{Origin: audit.Origin{Source: audit.SourceImport, Actor: importActor(*actor), Comment: *comment}, Force: *force}
	boot.ModelsDir = imp.Config.ModelsDir
	svc := cfgsvc.New(ctx, cfgsvc.Options{Store: cs, Boot: boot, Parse: opts})
	res, err := svc.Import(ctx, o, imp.Doc, *replace)
	switch {
	case errors.Is(err, cfgstore.ErrNotEmpty):
		fmt.Fprintf(stderr, "the database in %s already has a configuration (revision %d); use --replace to replace it\n", boot.DataDir, svc.Revision())
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "import failed:", err)
		return 1
	}
	if !res.Changed {
		fmt.Fprintf(stdout, "the database already holds exactly this configuration (revision %d); nothing to change\n", res.Revision)
	}
	now := time.Now().UnixMilli()
	for _, b := range imp.Benchmarks {
		if err := st.SeedBenchmark(b, now); err != nil {
			fmt.Fprintln(stderr, "benchmark", b.ID+":", err)
			return 1
		}
		o.Origin.Comment = "benchmark " + b.ID + " from the imported file"
		recordBench(ctx, cs, o.Origin, "benchmark", b.ID)
	}
	for _, su := range imp.Suites {
		if err := st.SeedSuite(su, now); err != nil {
			fmt.Fprintln(stderr, "suite", su.ID+":", err)
			return 1
		}
		o.Origin.Comment = "suite " + su.ID + " from the imported file"
		recordBench(ctx, cs, o.Origin, "suite", su.ID)
	}
	fmt.Fprintf(stdout, "imported %s as revision %d into %s\n", summary, res.Revision, st.Path())
	return 0
}

func recordBench(ctx context.Context, cs *cfgstore.Store, o audit.Origin, kind, id string) {
	e := audit.Entry{Action: audit.ActionImport, Section: audit.SectionBench, Target: kind + ":" + id, Before: []byte("null"), After: []byte("null")}
	o.Apply(&e)
	_, _ = cs.Record(ctx, e)
}

func importActor(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	for _, k := range []string{"USER", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return "cli:" + v
		}
	}
	return "cli"
}
