// Command baseline reads a running AWS account, reviews what it finds, compares
// two accounts, and generates a starting-point CloudFormation template.
//
//	baseline capture  -profile msnbc-dev  -prefix release -label dev  -out dev.json
//	baseline review   -in dev.json
//	baseline compare  -a dev.json -b prod.json
//	baseline generate -in dev.json -out generated.yaml
//
// Exit codes, so this can gate a pipeline:
//
//	0  nothing blocking
//	1  at least one BLOCKER finding, or a blocking difference between accounts
//	2  warnings only
//	3  the tool itself failed
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/vamsiatluri/releasebot-migration/internal/baseline"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "capture":
		capture(os.Args[2:])
	case "review":
		review(os.Args[2:])
	case "compare":
		compare(os.Args[2:])
	case "generate":
		generate(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `baseline — read a running AWS account into something you can diff and review

  capture   -profile P [-region R] [-prefix S] -label L -out FILE
            Read the account. Never reads secret VALUES; records key names only.

  review    -in FILE
            What a person should look at before copying this anywhere:
            resources no stack owns, secrets in environment variables, log
            groups that will outlive the decommission.

  compare   -a FILE -b FILE
            The point of the tool. Differences between two accounts, with the
            things that legitimately differ (account ids, API ids) normalised
            away so the real ones are visible.

  generate  -in FILE [-out FILE]
            A STARTING-POINT template. Read it, delete what should not come
            across, then deploy to a sandbox and compare until the only
            differences left are intended.
`)
	os.Exit(3)
}

func flagset(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

func capture(args []string) {
	fs := flagset("capture")
	profile := fs.String("profile", "", "AWS CLI profile")
	region := fs.String("region", "us-east-1", "region")
	prefix := fs.String("prefix", "", "only capture resources whose name contains this")
	label := fs.String("label", "capture", "label for reports")
	out := fs.String("out", "", "write JSON here (default: stdout)")
	verbose := fs.Bool("v", false, "print each AWS call")
	_ = fs.Parse(args)

	c := &baseline.Capturer{Profile: *profile, Region: *region, Prefix: *prefix, Verbose: *verbose}
	b, err := c.Capture(*label)
	if err != nil {
		die(err)
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	if *out == "" {
		os.Stdout.Write(append(data, '\n'))
	} else if err := os.WriteFile(*out, append(data, '\n'), 0o600); err != nil {
		die(err)
	} else {
		fmt.Printf("captured account %s -> %s (%d functions, %d roles, %d APIs)\n",
			b.Account, *out, len(b.Functions), len(b.Roles), len(b.APIs))
	}
	exitOn(baseline.Review(b))
}

func review(args []string) {
	fs := flagset("review")
	in := fs.String("in", "", "baseline JSON")
	_ = fs.Parse(args)
	b := load(*in)
	f := baseline.Review(b)
	baseline.WriteReview(os.Stdout, b, f)
	exitOn(f)
}

func compare(args []string) {
	fs := flagset("compare")
	a := fs.String("a", "", "first baseline JSON")
	bf := fs.String("b", "", "second baseline JSON")
	_ = fs.Parse(args)
	x, y := load(*a), load(*bf)
	d := baseline.Compare(x, y)
	baseline.WriteCompare(os.Stdout, x, y, d)
	for _, v := range d {
		if v.Severity == baseline.Blocker {
			os.Exit(1)
		}
	}
	if len(d) > 0 {
		os.Exit(2)
	}
}

func generate(args []string) {
	fs := flagset("generate")
	in := fs.String("in", "", "baseline JSON")
	out := fs.String("out", "", "write YAML here (default: stdout)")
	_ = fs.Parse(args)
	b := load(*in)
	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			die(err)
		}
		defer f.Close()
		w = f
	}
	baseline.Generate(w, b, baseline.Review(b))
	if *out != "" {
		fmt.Printf("wrote %s — read it before applying any of it\n", *out)
	}
}

func load(path string) *baseline.Baseline {
	if path == "" {
		die(fmt.Errorf("a baseline file is required"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		die(err)
	}
	var b baseline.Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		die(err)
	}
	return &b
}

func exitOn(f []baseline.Finding) {
	for _, x := range f {
		if x.Severity == baseline.Blocker {
			os.Exit(1)
		}
	}
	for _, x := range f {
		if x.Severity == baseline.Warn {
			os.Exit(2)
		}
	}
}

func die(err error) { fmt.Fprintln(os.Stderr, "baseline:", err); os.Exit(3) }
