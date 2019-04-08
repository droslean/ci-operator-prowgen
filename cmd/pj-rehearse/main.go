package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	pjapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config/secret"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	prowgithub "k8s.io/test-infra/prow/github"
	pjdwapi "k8s.io/test-infra/prow/pod-utils/downwardapi"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/openshift/ci-operator-prowgen/pkg/config"
	"github.com/openshift/ci-operator-prowgen/pkg/diffs"
	"github.com/openshift/ci-operator-prowgen/pkg/rehearse"
)

func loadClusterConfig() (*rest.Config, error) {
	clusterConfig, err := rest.InClusterConfig()
	if err == nil {
		return clusterConfig, nil
	}

	credentials, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return nil, fmt.Errorf("could not load credentials from config: %v", err)
	}

	clusterConfig, err = clientcmd.NewDefaultClientConfig(*credentials, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("could not load client configuration: %v", err)
	}
	return clusterConfig, nil
}

type options struct {
	dryRun       bool
	noFail       bool
	local        bool
	allowVolumes bool
	debugLogPath string
	metricsPath  string
	github       prowflagutil.GitHubOptions

	releaseRepoPath string
	rehearsalLimit  int
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.BoolVar(&o.dryRun, "dry-run", true, "Whether to actually submit rehearsal jobs to Prow")
	fs.BoolVar(&o.noFail, "no-fail", true, "Whether to actually end unsuccessfuly when something breaks")
	fs.BoolVar(&o.local, "local", false, "Whether this is a local execution or part of a CI job")
	fs.BoolVar(&o.allowVolumes, "allow-volumes", false, "Allows jobs with extra volumes to be rehearsed")

	fs.StringVar(&o.debugLogPath, "debug-log", "", "Alternate file for debug output, defaults to stderr")
	fs.StringVar(&o.releaseRepoPath, "candidate-path", "", "Path to a openshift/release working copy with a revision to be tested")
	fs.StringVar(&o.metricsPath, "metrics-output", "", "Path to a file where JSON metrics will be dumped after rehearsal")

	fs.IntVar(&o.rehearsalLimit, "rehearsal-limit", 15, "Upper limit of jobs attempted to rehearse (if more jobs would be rehearsed, none will)")

	o.github.AddFlags(fs)
	fs.Parse(os.Args[1:])
	return o
}

func validateOptions(o options) error {
	if len(o.releaseRepoPath) == 0 {
		return fmt.Errorf("--candidate-path was not provided")
	}
	return nil
}

const (
	misconfigurationOutput = `[ERROR] pj-rehearse: misconfiguration

pj-rehearse could not process its necessary inputs properly. No rehearsal
jobs were run. This is likely a pj-rehearse job configuration problem.`
	rehearseFailureOutput = `[ERROR] pj-rehearse: rehearsal tool failure

pj-rehearse attempted to submit jobs for rehearsal, but it failed to either
submit them or to fetch their results. This is either a pj-rehearse bug or
an infrastructure issue.`
	jobsFailureOutput = `[ERROR] pj-rehearse: rehearsed jobs failure

pj-rehearse rehearsed jobs and at least one of them failed. This means that
job would fail when executed against the current HEAD of the target branch.`
	failedSetupOutput = `[ERROR] pj-rehearse: setup failure

pj-rehearse failed to finish all setup necessary to perform job rehearsals.
This is either a pj-rehearse bug or an infrastructure failure.`
)

func gracefulExit(suppressFailures bool, message string) int {
	if message != "" {
		fmt.Fprintln(os.Stderr, message)
	}

	if suppressFailures {
		return 0
	}
	return 1
}

func rehearseMain() int {
	o := gatherOptions()
	err := validateOptions(o)
	if err != nil {
		logrus.WithError(err).Fatal("invalid options")
		return gracefulExit(o.noFail, misconfigurationOutput)
	}

	metrics := rehearse.NewMetrics(o.metricsPath)
	defer metrics.Dump()

	var jobSpec *pjdwapi.JobSpec
	if o.local {
		if jobSpec, err = config.NewLocalJobSpec(o.releaseRepoPath); err != nil {
			logrus.WithError(err).Error("could not create local JobSpec")
			gracefulExit(o.noFail, misconfigurationOutput)
		}
	} else {
		if jobSpec, err = pjdwapi.ResolveSpecFromEnv(); err != nil {
			logrus.WithError(err).Error("could not read JOB_SPEC")
			return gracefulExit(o.noFail, misconfigurationOutput)
		}
	}

	prFields := logrus.Fields{prowgithub.OrgLogField: jobSpec.Refs.Org, prowgithub.RepoLogField: jobSpec.Refs.Repo}
	metrics.Org = jobSpec.Refs.Org
	metrics.Repo = jobSpec.Refs.Repo
	logger := logrus.WithFields(prFields)

	if jobSpec.Type != pjapi.PresubmitJob {
		logger.Info("Not able to rehearse jobs when not run in the context of a presubmit job")
		// Exiting successfully will make pj-rehearsal job not fail when run as a
		// in a batch job. Such failures would be confusing and unactionable
		return 0
	}

	prNumber := jobSpec.Refs.Pulls[0].Number
	if o.local {
		prNumber = int(time.Now().Unix())
	}
	metrics.Pr = prNumber
	logger = logrus.WithField(prowgithub.PrLogField, prNumber)
	logger.Info("Rehearsing Prow jobs for a configuration PR")

	var clusterConfig *rest.Config
	if !o.dryRun {
		clusterConfig, err = loadClusterConfig()
		if err != nil {
			logger.WithError(err).Error("could not load cluster clusterConfig")
			return gracefulExit(o.noFail, misconfigurationOutput)
		}
	}

	prConfig := config.GetAllConfigs(o.releaseRepoPath, logger)
	masterConfig, err := config.GetAllConfigsFromSHA(o.releaseRepoPath, jobSpec.Refs.BaseSHA, logger)
	if err != nil {
		logger.WithError(err).Error("could not load configuration from base revision of release repo")
		return gracefulExit(o.noFail, misconfigurationOutput)
	}

	// We always need both Prow config versions, otherwise we cannot compare them
	if masterConfig.Prow == nil || prConfig.Prow == nil {
		logger.WithError(err).Error("could not load Prow configs from base or tested revision of release repo")
		return gracefulExit(o.noFail, misconfigurationOutput)
	}
	// We always need PR versions of templates and ciop config, otherwise we cannot provide them to rehearsed jobs
	if prConfig.Templates == nil || prConfig.CiOperator == nil {
		logger.WithError(err).Error("could not load template/ci-operator configs from tested revision of release repo")
		return gracefulExit(o.noFail, misconfigurationOutput)
	}

	// We can only detect changes if we managed to load both ci-operator config versions
	changedCiopConfigs := config.CompoundCiopConfig{}
	affectedJobs := make(map[string]sets.String)
	if masterConfig.CiOperator != nil && prConfig.CiOperator != nil {
		changedCiopConfigs, affectedJobs = diffs.GetChangedCiopConfigs(masterConfig.CiOperator, prConfig.CiOperator, logger)
		metrics.RecordChangedCiopConfigs(changedCiopConfigs)
	}

	changedTemplates := make(config.CiTemplates)
	// We can only detect changes if we managed to load both CI template versions
	if masterConfig.Templates != nil && prConfig.Templates != nil {
		changedTemplates = diffs.GetChangedTemplates(masterConfig.Templates, prConfig.Templates, logger)
		metrics.RecordChangedTemplates(changedTemplates)
	}
	changedClusterProfiles, err := config.GetChangedClusterProfiles(o.releaseRepoPath, jobSpec.Refs.BaseSHA)
	if err != nil {
		logger.WithError(err).Error("could not get cluster profile differences")
		return gracefulExit(o.noFail, misconfigurationOutput)
	}
	if len(changedClusterProfiles) != 0 {
		logger.WithField("profiles", changedClusterProfiles).Info("cluster profiles changed")
		metrics.RecordChangedClusterProfiles(changedClusterProfiles)
	}

	namespace := prConfig.Prow.ProwJobNamespace
	if o.local {
		namespace = "ci-stg"
	}

	cmClient, err := rehearse.NewCMClient(clusterConfig, namespace, o.dryRun)
	if err != nil {
		logger.WithError(err).Error("could not create a configMap client")
		return gracefulExit(o.noFail, misconfigurationOutput)
	}

	cmManager := config.NewTemplateCMManager(cmClient, prNumber, logger, changedTemplates)
	defer func() {
		if err := cmManager.CleanupCMTemplates(); err != nil {
			logger.WithError(err).Error("failed to clean up temporary template CM")
		}
	}()
	if err := cmManager.CreateCMTemplates(); err != nil {
		logger.WithError(err).Error("couldn't create template configMap")
		return gracefulExit(o.noFail, failedSetupOutput)
	}
	if err := cmManager.CreateClusterProfiles(filepath.Join(o.releaseRepoPath, config.ClusterProfilesPath), changedClusterProfiles); err != nil {
		logger.WithError(err).Error("couldn't create cluster profile ConfigMaps")
		return gracefulExit(o.noFail, failedSetupOutput)
	}

	pjclient, err := rehearse.NewProwJobClient(clusterConfig, namespace, o.dryRun)
	if err != nil {
		logger.WithError(err).Error("could not create a ProwJob client")
		return gracefulExit(o.noFail, failedSetupOutput)
	}

	debugLogger := logrus.New()
	debugLogger.Level = logrus.DebugLevel
	if o.debugLogPath != "" {
		if f, err := os.OpenFile(o.debugLogPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, os.ModePerm); err == nil {
			defer f.Close()
			debugLogger.Out = f
		} else {
			logger.WithError(err).Error("could not open debug log file")
			return gracefulExit(o.noFail, failedSetupOutput)
		}
	}
	loggers := rehearse.Loggers{Job: logger, Debug: debugLogger.WithField(prowgithub.PrLogField, prNumber)}

	toRehearse := diffs.GetChangedPresubmits(masterConfig.Prow, prConfig.Prow, logger)
	metrics.RecordChangedPresubmits(toRehearse)
	metrics.RecordOpportunity(toRehearse, "direct-change")

	presubmitsWithChangedCiopConfigs := diffs.GetPresubmitsForCiopConfigs(prConfig.Prow, changedCiopConfigs, logger, affectedJobs)
	metrics.RecordOpportunity(presubmitsWithChangedCiopConfigs, "ci-operator-config-change")
	toRehearse.AddAll(presubmitsWithChangedCiopConfigs)

	presubmitsWithChangedTemplates := rehearse.AddRandomJobsForChangedTemplates(changedTemplates, prConfig.Prow.JobConfig.Presubmits, loggers, prNumber)
	metrics.RecordOpportunity(presubmitsWithChangedTemplates, "templates-change")
	toRehearse.AddAll(presubmitsWithChangedTemplates)

	toRehearseClusterProfiles := diffs.GetPresubmitsForClusterProfiles(prConfig.Prow, changedClusterProfiles, logger)
	metrics.RecordOpportunity(toRehearseClusterProfiles, "cluster-profile-change")
	toRehearse.AddAll(toRehearseClusterProfiles)

	rehearsals := rehearse.ConfigureRehearsalJobs(toRehearse, prConfig.CiOperator, prNumber, loggers, o.allowVolumes, changedTemplates, changedClusterProfiles)
	metrics.RecordActual(rehearsals)
	if len(rehearsals) == 0 {
		logger.Info("no jobs to rehearse have been found")
		return 0
	} else if len(rehearsals) > o.rehearsalLimit {
		jobCountFields := logrus.Fields{
			"rehearsal-threshold": o.rehearsalLimit,
			"rehearsal-jobs":      len(rehearsals),
		}
		logger.WithFields(jobCountFields).Info("Would rehearse too many jobs, will not proceed")
		return 0
	}

	secretAgent := &secret.Agent{}
	if !o.dryRun {
		if err := secretAgent.Start([]string{o.github.TokenPath}); err != nil {
			logrus.WithError(err).Fatal("Error starting secrets agent.")
		}
	}

	githubClient, err := o.github.GitHubClient(secretAgent, o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting GitHub client.")
	}

	if err := rehearse.RetireStallStatuses(githubClient, jobSpec.Refs.Org, jobSpec.Refs.Repo, jobSpec.Refs.Pulls[0].SHA, rehearsals); err != nil {
		logrus.WithError(err).Warn("couldn't retire stall statuses")
	}

	executor := rehearse.NewExecutor(rehearsals, prNumber, o.releaseRepoPath, jobSpec.Refs, o.dryRun, loggers, pjclient)
	success, err := executor.ExecuteJobs()
	metrics.Execution = executor.Metrics
	if err != nil {
		logger.WithError(err).Error("Failed to rehearse jobs")
		return gracefulExit(o.noFail, rehearseFailureOutput)
	}
	if !success {
		logger.Error("Some jobs failed their rehearsal runs")
		return gracefulExit(o.noFail, jobsFailureOutput)
	}
	logger.Info("All jobs were rehearsed successfully")
	return 0
}

func main() {
	os.Exit(rehearseMain())
}
