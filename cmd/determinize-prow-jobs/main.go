package main

import (
	"flag"
	"fmt"
	"os"

	jc "github.com/openshift/ci-operator-prowgen/pkg/jobconfig"
)

type options struct {
	prowJobConfigDir string
	help             bool
}

func bindOptions(flag *flag.FlagSet) *options {
	opt := &options{}

	flag.StringVar(&opt.prowJobConfigDir, "prow-jobs-dir", "", "Path to a root of directory structure with Prow job config files (ci-operator/jobs in openshift/release)")
	flag.BoolVar(&opt.help, "h", false, "Show help for ci-operator-prowgen")

	return opt
}

func main() {
	flagSet := flag.NewFlagSet("", flag.ExitOnError)
	opt := bindOptions(flagSet)
	flagSet.Parse(os.Args[1:])

	if opt.help {
		flagSet.Usage()
		os.Exit(0)
	}

	if len(opt.prowJobConfigDir) == 0 {
		fmt.Fprintln(os.Stderr, "determinize tool needs the --prow-jobs-dir")
		os.Exit(1)
	}

	if err := jc.DeterminizeJobs(opt.prowJobConfigDir); err != nil {
		fmt.Fprintf(os.Stderr, "determinize failed (%v)\n", err)
	}
}
