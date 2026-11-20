// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aisathome/aisathome/node/modelcheck"
	"github.com/aisathome/aisathome/node/setup"
)

// Exit codes of `aisathome-node modelcheck`.
const (
	mcPass    = 0
	mcFail    = 1
	mcCannot  = 2 // could not check: no engine, no reference, missing answers, bad flags
	mcDefault = "http://127.0.0.1:8000"
)

const modelcheckUsage = `Usage:
  aisathome-node modelcheck [flags]           check the local engine against a reference
  aisathome-node modelcheck record -out FILE  record a reference (maintainers, at a release)
  aisathome-node modelcheck compare -ref REF -run FILE   compare a saved run, offline
  aisathome-node modelcheck list              list the built-in references

It sends 100 short prompts to the local llama-server (greedy, top-20 logprobs, thinking off)
and compares the answers with a reference recorded on a known llama.cpp build, model file and
GPU. Exit code: 0 pass, 1 fail, 2 could not check. Run "aisathome-node modelcheck -h" for
the flags.
`

// mcEnv is what the model check needs from the machine; tests replace it.
type mcEnv struct {
	smi  setup.SMI
	http *http.Client // for -ref URLs
	now  func() time.Time
	hash func(path string) (string, error)
	// retry is the wait before asking the engine again (0: the client's default).
	retry time.Duration
}

func runModelcheck(args []string, stdout, stderr io.Writer) int {
	env := mcEnv{smi: setup.ExecSMI{}, now: time.Now, hash: hashFile}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return env.main(ctx, args, stdout, stderr)
}

func (e mcEnv) main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "record":
			return e.record(ctx, args[1:], stdout, stderr)
		case "compare":
			return e.compare(ctx, args[1:], stdout, stderr)
		case "list":
			return e.list(stdout, stderr)
		case "help":
			fmt.Fprint(stdout, modelcheckUsage)
			return 0
		}
	}
	return e.check(ctx, args, stdout, stderr)
}

// thresholdFlags adds the limits' flags, defaulting to the published ones.
func thresholdFlags(fl *flag.FlagSet) *modelcheck.Thresholds {
	th := modelcheck.DefaultThresholds
	fl.Float64Var(&th.MaxCheckKL, "max-kl", th.MaxCheckKL, "limit for the KL divergence of a group of 4 prompts")
	fl.IntVar(&th.MaxFlips, "max-flips", th.MaxFlips, "limit for confident flips")
	fl.Float64Var(&th.Margin, "margin", th.Margin, "a flip is confident when the reference led by more than this (nats)")
	return &th
}

// serverFlags adds -server and -allow-remote.
func serverFlags(fl *flag.FlagSet) (*string, *bool) {
	return fl.String("server", mcDefault, "the llama-server to check"),
		fl.Bool("allow-remote", false, "allow a -server that is not on this computer (for example an engine in a container)")
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fl := flag.NewFlagSet("modelcheck "+name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() {
		fmt.Fprint(stderr, modelcheckUsage, "\nFlags of ", fl.Name(), ":\n")
		fl.PrintDefaults()
	}
	return fl
}

func (e mcEnv) client(server string, allowRemote bool) (*modelcheck.Client, error) {
	if !modelcheck.IsLocal(server) && !allowRemote {
		return nil, fmt.Errorf("-server %s is not on this computer; the check sends prompts only to a local engine (add -allow-remote for an engine in a container or on your network)", server)
	}
	return &modelcheck.Client{BaseURL: server, Retry: e.retry}, nil
}

func (e mcEnv) props(ctx context.Context, c *modelcheck.Client) (modelcheck.Props, error) {
	p, err := c.Props(ctx)
	if err != nil {
		return p, fmt.Errorf("cannot read %s/props: %w. Is the node's engine (llama-server) running there?", c.BaseURL, err)
	}
	return p, nil
}

// gpu is the machine's card: name, else nvidia-smi's, when there is exactly one.
func (e mcEnv) gpu(ctx context.Context, name string) (modelcheck.GPU, error) {
	if name != "" {
		return modelcheck.GPU{Name: name}, nil
	}
	gpus, err := setup.ReadGPUs(ctx, e.smi)
	if err != nil {
		return modelcheck.GPU{}, err
	}
	if len(gpus) != 1 {
		return modelcheck.GPU{}, fmt.Errorf("%d GPUs; name the one the engine runs on with -gpu", len(gpus))
	}
	return modelcheck.GPU{Name: gpus[0].Name, Cap: gpus[0].Cap}, nil
}

// resolveRef is a built-in reference by name, a file, or an http(s) URL (which needs sha).
func (e mcEnv) resolveRef(ctx context.Context, spec, sha string) (*modelcheck.Reference, error) {
	for _, b := range modelcheck.Builtins {
		if b.Name == spec {
			return modelcheck.LoadBuiltin(spec)
		}
	}
	if strings.HasPrefix(spec, "https://") || strings.HasPrefix(spec, "http://") {
		return modelcheck.Fetch(ctx, e.http, spec, sha)
	}
	return modelcheck.LoadFile(spec, sha)
}

func builtins() ([]*modelcheck.Reference, error) {
	var refs []*modelcheck.Reference
	for _, b := range modelcheck.Builtins {
		r, err := modelcheck.LoadBuiltin(b.Name)
		if err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// runOf is a run's file: the results with what the server says about itself.
func runOf(p modelcheck.Props, gpu modelcheck.GPU, rq modelcheck.Request, now time.Time, results []modelcheck.Result) *modelcheck.Reference {
	tag, commit := p.Build()
	return &modelcheck.Reference{
		Format: modelcheck.Format, Kind: modelcheck.KindRun, Name: "run", Created: now.UTC().Format("2006-01-02"),
		Engine:  modelcheck.Engine{Name: "llama.cpp", Build: tag, Commit: commit, BuildInfo: p.BuildInfo},
		Model:   modelcheck.Model{File: p.ModelFile(), Ftype: p.ModelFtype},
		GPU:     gpu,
		Server:  modelcheck.Server{Ctx: p.Gen.NCtx, Slots: p.TotalSlots},
		Request: rq, Results: results,
	}
}

func progress(stderr io.Writer) func(done, total int) {
	return func(done, total int) {
		if done%25 == 0 || done == total {
			fmt.Fprintf(stderr, "  %d/%d prompts\n", done, total)
		}
	}
}

func writeRef(path string, r *modelcheck.Reference, mode os.FileMode) (string, error) {
	b, err := r.Encode(strings.HasSuffix(path, ".gz"))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, mode); err != nil {
		return "", err
	}
	return modelcheck.SHA256(b), nil
}

// verdictCode prints the report and returns the exit code.
func verdictCode(ref, run *modelcheck.Reference, rep modelcheck.Report, asJSON bool, stdout io.Writer) int {
	if asJSON {
		out := map[string]any{"reference": ref.Name, "reference_label": ref.Label(), "run_label": run.Label(), "report": rep}
		b, _ := json.MarshalIndent(out, "", " ")
		fmt.Fprintln(stdout, string(b))
	} else {
		modelcheck.WriteText(stdout, ref, run, rep)
	}
	switch rep.Verdict {
	case modelcheck.Pass:
		return mcPass
	case modelcheck.Fail:
		return mcFail
	}
	return mcCannot
}

// check is `aisathome-node modelcheck`: run the reference's prompts on the engine and compare.
func (e mcEnv) check(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := newFlags("", stderr)
	server, allowRemote := serverFlags(fl)
	refSpec := fl.String("ref", "", "reference: a built-in name (see list), a file, or an https URL with -ref-sha256 (default: the built-in one for the model the engine serves)")
	refSHA := fl.String("ref-sha256", "", "the reference file's SHA-256 (required for a URL)")
	limit := fl.Int("limit", 0, "only the first N prompts (at least 4; default all)")
	out := fl.String("out", "", "also save this run to FILE (the reference format; .gz to compress), to send or compare later")
	gpuName := fl.String("gpu", "", "the GPU's name for the saved run (default: from nvidia-smi)")
	asJSON := fl.Bool("json", false, "print the result as JSON")
	th := thresholdFlags(fl)
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return mcCannot
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "modelcheck: unexpected %q\n", fl.Arg(0))
		fl.Usage()
		return mcCannot
	}
	c, err := e.client(*server, *allowRemote)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck:", err)
		return mcCannot
	}
	p, err := e.props(ctx, c)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck:", err)
		return mcCannot
	}
	var ref *modelcheck.Reference
	if *refSpec == "" {
		refs, err := builtins()
		if err == nil {
			ref, err = modelcheck.Pick(refs, p)
		}
		if err != nil {
			fmt.Fprintln(stderr, "modelcheck:", err)
			return mcCannot
		}
	} else if ref, err = e.resolveRef(ctx, *refSpec, *refSHA); err != nil {
		fmt.Fprintln(stderr, "modelcheck: reference:", err)
		return mcCannot
	}
	var prompts []modelcheck.Prompt
	for _, r := range ref.Results {
		prompts = append(prompts, modelcheck.Prompt{ID: r.ID, Text: r.Text})
	}
	if *limit > 0 && *limit < len(prompts) {
		prompts = prompts[:*limit]
	}
	gpu, _ := e.gpu(ctx, *gpuName) // only a label here
	fmt.Fprintf(stderr, "Checking %s against %s: %d prompts of up to %d tokens.\n", *server, ref.Name, len(prompts), ref.Request.MaxTokens)
	results := c.Run(ctx, prompts, ref.Request, progress(stderr))
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "modelcheck: stopped")
		return mcCannot
	}
	run := runOf(p, gpu, ref.Request, e.now(), results)
	if *out != "" {
		if _, err := writeRef(*out, run, 0o644); err != nil {
			fmt.Fprintln(stderr, "modelcheck:", err)
		} else {
			fmt.Fprintf(stderr, "Saved this run to %s\n", *out)
		}
	}
	return verdictCode(ref, run, modelcheck.Compare(ref, run, *th), *asJSON, stdout)
}

// compare is `aisathome-node modelcheck compare`: a saved run against a reference, offline.
func (e mcEnv) compare(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := newFlags("compare", stderr)
	refSpec := fl.String("ref", "", "reference: a built-in name, a file, or an https URL with -ref-sha256")
	refSHA := fl.String("ref-sha256", "", "the reference file's SHA-256 (required for a URL)")
	runFile := fl.String("run", "", "the saved run (modelcheck -out), or another reference")
	asJSON := fl.Bool("json", false, "print the result as JSON")
	th := thresholdFlags(fl)
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return mcCannot
	}
	if *refSpec == "" || *runFile == "" || fl.NArg() > 0 {
		fmt.Fprintln(stderr, "modelcheck compare: needs -ref and -run")
		return mcCannot
	}
	ref, err := e.resolveRef(ctx, *refSpec, *refSHA)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck: reference:", err)
		return mcCannot
	}
	run, err := modelcheck.LoadFile(*runFile, "")
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck: run:", err)
		return mcCannot
	}
	return verdictCode(ref, run, modelcheck.Compare(ref, run, *th), *asJSON, stdout)
}

func (e mcEnv) list(stdout, stderr io.Writer) int {
	refs, err := builtins()
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck:", err)
		return mcCannot
	}
	for i, r := range refs {
		fmt.Fprintf(stdout, "%s\n  %s, recorded %s, sha256 %s\n", r.Name, r.Label(), r.Created, modelcheck.Builtins[i].SHA256)
		if r.Note != "" {
			fmt.Fprintf(stdout, "  %s\n", r.Note)
		}
	}
	return 0
}

var reSlug = regexp.MustCompile(`[^a-z0-9.]+`)

// refName is a reference's default name: model, llama.cpp build, GPU and speculation, as
// "qwen3.8-27b-iq4xs-b12342-rtx4090".
func refName(modelFile, build, gpu, spec string) string {
	m := strings.ToLower(strings.TrimSuffix(modelFile, ".gguf"))
	m = strings.ReplaceAll(strings.ReplaceAll(m, "-ud-", "-"), "_", "")
	g := strings.ToLower(gpu)
	for _, s := range []string{"nvidia", "geforce"} {
		g = strings.ReplaceAll(g, s, "")
	}
	g = reSlug.ReplaceAllString(g, "")
	name := reSlug.ReplaceAllString(m, "-") + "-" + build + "-" + g
	if spec != "" && spec != setup.SpecOff {
		name += "-" + spec
	}
	return strings.Trim(name, "-")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// record is `aisathome-node modelcheck record`: the release process records a reference on
// the release's engine, model file and a known GPU (docs/modelcheck.md).
func (e mcEnv) record(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := newFlags("record", stderr)
	server, allowRemote := serverFlags(fl)
	out := fl.String("out", "", "the reference file to write (.json.gz)")
	name := fl.String("name", "", "the reference's name (default: model-build-gpu, as qwen3.8-27b-iq4xs-b12342-rtx4090)")
	note := fl.String("note", "", "a note for the people who check against it")
	gpuName := fl.String("gpu", "", "the GPU's name (default: from nvidia-smi, if the machine has one GPU)")
	build := fl.String("engine-build", "", "the llama.cpp release tag (bNNNN) when /props does not name it")
	model := fl.String("model", "", "the model file to hash (default: the engine's, from /props)")
	modelSHA := fl.String("model-sha256", "", "the model file's SHA-256, instead of hashing it")
	tierName := fl.String("tier", "", "the node tier the engine runs (default: from the model file)")
	spec := fl.String("speculation", setup.SpecOff, "the engine's speculative decoding: off, mtp or dflash2")
	limit := fl.Int("limit", 0, "only the first N prompts (for testing; a published reference has all)")
	promptsFile := fl.String("prompts", "", "a private prompt set (JSON list of {id, text}) instead of the public one: the gateway's secret model-check reference, never published")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return mcCannot
	}
	if *out == "" || fl.NArg() > 0 {
		fmt.Fprintln(stderr, "modelcheck record: needs -out FILE")
		return mcCannot
	}
	sp, err := setup.NormSpec(*spec)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck record:", err)
		return mcCannot
	}
	c, err := e.client(*server, *allowRemote)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck record:", err)
		return mcCannot
	}
	p, err := e.props(ctx, c)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck record:", err)
		return mcCannot
	}
	tag, commit := p.Build()
	if *build != "" {
		if _, ok := setup.LlamaBuildNumber(*build); !ok {
			fmt.Fprintf(stderr, "modelcheck record: -engine-build %q: want a release tag such as b12342\n", *build)
			return mcCannot
		}
		if tag != "" && tag != *build {
			fmt.Fprintf(stderr, "modelcheck record: -engine-build %s, but the engine says %s\n", *build, p.BuildInfo)
			return mcCannot
		}
		tag = *build
	}
	if tag == "" {
		fmt.Fprintf(stderr, "modelcheck record: the engine's build_info %q names no llama.cpp release; pass -engine-build bNNNN\n", p.BuildInfo)
		return mcCannot
	}
	sha := strings.ToLower(*modelSHA)
	if sha == "" {
		path := *model
		if path == "" {
			path = p.ModelPath
		}
		fmt.Fprintf(stderr, "Hashing %s ...\n", path)
		if sha, err = e.hash(path); err != nil {
			fmt.Fprintf(stderr, "modelcheck record: model file: %v (pass -model PATH or -model-sha256)\n", err)
			return mcCannot
		}
	}
	gpu, err := e.gpu(ctx, *gpuName)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck record: GPU:", err)
		return mcCannot
	}
	srv := modelcheck.Server{Tier: *tierName, Ctx: p.Gen.NCtx, Slots: p.TotalSlots, Speculation: sp}
	for _, t := range setup.Tiers {
		if (srv.Tier == "" && strings.EqualFold(t.Model.File(), p.ModelFile())) || srv.Tier == t.Name {
			srv.Tier, srv.KV = t.Name, t.KV
			if !strings.EqualFold(t.Model.SHA256, sha) {
				fmt.Fprintf(stderr, "warning: the model file's SHA-256 %s is not the %s tier's pinned %s\n", sha, t.Name, t.Model.SHA256)
			}
		}
	}
	prompts := modelcheck.Prompts()
	if *promptsFile != "" {
		if prompts, err = modelcheck.LoadPrompts(*promptsFile); err != nil {
			fmt.Fprintln(stderr, "modelcheck record:", err)
			return mcCannot
		}
	}
	if *limit > 0 && *limit < len(prompts) {
		prompts = prompts[:*limit]
		fmt.Fprintf(stderr, "warning: -limit %d: a test reference, not one to publish\n", *limit)
	}
	if *name == "" {
		*name = refName(p.ModelFile(), tag, gpu.Name, sp)
	}
	fmt.Fprintf(stderr, "Recording %s on %s: %d prompts.\n", *name, *server, len(prompts))
	results := c.Run(ctx, prompts, modelcheck.DefaultRequest, progress(stderr))
	for _, r := range results {
		if r.Error != "" {
			fmt.Fprintf(stderr, "modelcheck record: prompt %s: %s; no reference written\n", r.ID, r.Error)
			return mcFail
		}
	}
	if len(results) < len(prompts) {
		fmt.Fprintln(stderr, "modelcheck record: stopped; no reference written")
		return mcCannot
	}
	ref := &modelcheck.Reference{
		Format: modelcheck.Format, Kind: modelcheck.KindReference, Name: *name,
		Created: e.now().UTC().Format("2006-01-02"), Note: *note,
		Engine: modelcheck.Engine{Name: "llama.cpp", Build: tag, Commit: commit, BuildInfo: p.BuildInfo},
		Model:  modelcheck.Model{File: p.ModelFile(), SHA256: sha, Ftype: p.ModelFtype},
		GPU:    gpu, Server: srv, Request: modelcheck.DefaultRequest, Results: results,
	}
	if err := ref.Validate(); err != nil {
		fmt.Fprintln(stderr, "modelcheck record:", err)
		return mcFail
	}
	mode := os.FileMode(0o644)
	if *promptsFile != "" { // the secret pool and its answers: readable by its owner only
		mode = 0o600
	}
	sum, err := writeRef(*out, ref, mode)
	if err != nil {
		fmt.Fprintln(stderr, "modelcheck record:", err)
		return mcCannot
	}
	fmt.Fprintf(stdout, "Wrote %s\n  %s\n  sha256 %s\n", *out, ref.Label(), sum)
	if *promptsFile != "" { // a secret set: never shipped in the node program or published
		fmt.Fprintf(stdout, "A reference of a private prompt set: keep it with the gateway's secrets (model_check.references), never in a repository.\n")
		return mcPass
	}
	// The built-ins are the gzipped files themselves, pinned by the SHA-256 of those bytes.
	if file := filepath.Base(*out); strings.HasSuffix(file, ".json.gz") {
		fmt.Fprintf(stdout, "To ship it, copy it to node/modelcheck/references/ and add to modelcheck.Builtins:\n")
		fmt.Fprintf(stdout, "\t{Name: %q, File: %q,\n\t\tSHA256: %q},\n", ref.Name, file, sum)
	} else {
		fmt.Fprintf(stdout, "Built-in references are gzipped (.json.gz): gzip this file and pin the SHA-256 of the .gz to ship it.\n")
	}
	return mcPass
}
