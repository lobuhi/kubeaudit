package commands

import (
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	apiv1 "k8s.io/api/core/v1"

	"github.com/Shopify/kubeaudit"
	"github.com/Shopify/kubeaudit/auditors/all"
	"github.com/Shopify/kubeaudit/config"
	"github.com/Shopify/kubeaudit/internal/color"
	"github.com/Shopify/kubeaudit/internal/k8sinternal"
	"github.com/Shopify/kubeaudit/internal/sarif"
)

var rootConfig rootFlags

type rootFlags struct {
	format           string
	kubeConfig       string
	context          string
	manifest         string
	namespace        string
	minSeverity      string
	exitCode         int
	includeGenerated bool
	noColor          bool
}

// RootCmd defines the shell command usage for kubeaudit.
var RootCmd = &cobra.Command{
	Use:   "kubeaudit",
	Short: "A Kubernetes security auditor",
	Long: `Kubeaudit audits Kubernetes clusters for common security controls.

kubeaudit has three modes:
  1. Manifest mode: If a Kubernetes manifest file is provided using the -f/--manifest flag, kubeaudit will audit the manifest file. Kubeaudit also supports autofixing in manifest mode using the 'autofix' command. This will fix the manifest in-place. The fixed manifest can be written to a different file using the -o/--out flag.
  2. Cluster mode: If kubeaudit detects it is running in a cluster, it will audit the other resources in the cluster.
  3. Local mode: kubeaudit will try to connect to a cluster using the local kubeconfig file ($HOME/.kube/config). A different kubeconfig location can be specified using the -c/--kubeconfig flag
`,
}

// Execute is a wrapper for the RootCmd.Execute method which will exit the program if there is an error.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func init() {
	RootCmd.PersistentFlags().StringVarP(&rootConfig.kubeConfig, "kubeconfig", "", "", "Path to local Kubernetes config file. Only used in local mode (default is $HOME/.kube/config)")
	RootCmd.PersistentFlags().StringVarP(&rootConfig.context, "context", "c", "", "The name of the kubeconfig context to use")
	RootCmd.PersistentFlags().StringVarP(&rootConfig.minSeverity, "minseverity", "m", "info", "Set the lowest severity level to report (one of \"error\", \"warning\", \"info\")")
	RootCmd.PersistentFlags().StringVarP(&rootConfig.format, "format", "p", "pretty", "The output format to use (one of \"sarif\",\"pretty\", \"logrus\", \"json\")")
	RootCmd.PersistentFlags().StringVarP(&rootConfig.namespace, "namespace", "n", apiv1.NamespaceAll, "Only audit resources in the specified namespace. Not currently supported in manifest mode.")
	RootCmd.PersistentFlags().BoolVarP(&rootConfig.includeGenerated, "includegenerated", "g", false, "Include generated resources in scan  (eg. pods generated by deployments).")
	RootCmd.PersistentFlags().BoolVar(&rootConfig.noColor, "no-color", false, "Don't produce colored output.")
	RootCmd.PersistentFlags().StringVarP(&rootConfig.manifest, "manifest", "f", "", "Path to the yaml configuration to audit. Only used in manifest mode.")
	RootCmd.PersistentFlags().IntVarP(&rootConfig.exitCode, "exitcode", "e", 2, "Exit code to use if there are results with severity of \"error\". Conventionally, 0 is used for success and all non-zero codes for an error.")
}

// KubeauditLogLevels represents an enum for the supported log levels.
var KubeauditLogLevels = map[string]kubeaudit.SeverityLevel{
	"error":   kubeaudit.Error,
	"warn":    kubeaudit.Warn,
	"warning": kubeaudit.Warn,
	"info":    kubeaudit.Info,
}

func runAudit(auditable ...kubeaudit.Auditable) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		report := getReport(auditable...)

		fmt.Fprintln(os.Stderr, color.Yellow("\n[WARNING]: kubernetes.io for override labels will soon be deprecated. Please, update them to use kubeaudit.io instead."))

		printOptions := []kubeaudit.PrintOption{
			kubeaudit.WithMinSeverity(KubeauditLogLevels[strings.ToLower(rootConfig.minSeverity)]),
			kubeaudit.WithColor(!rootConfig.noColor),
		}

		switch rootConfig.format {
		case "sarif":
			sarifReport, err := sarif.Create(report)
			if err != nil {
				log.WithError(err).Fatal("Error generating the SARIF output")
			}
			sarifReport.PrettyWrite(os.Stdout)
			return
		case "json":
			printOptions = append(printOptions, kubeaudit.WithFormatter(&log.JSONFormatter{}))
		case "logrus":
			printOptions = append(printOptions, kubeaudit.WithFormatter(&log.TextFormatter{}))
		}

		report.PrintResults(printOptions...)

		if report.HasErrors() {
			os.Exit(rootConfig.exitCode)
		}
	}
}

func getReport(auditors ...kubeaudit.Auditable) *kubeaudit.Report {
	auditor := initKubeaudit(auditors...)

	if rootConfig.manifest != "" {
		var f *os.File
		if rootConfig.manifest == "-" {
			f = os.Stdin
			rootConfig.manifest = ""
		} else {
			manifest, err := os.Open(rootConfig.manifest)
			if err != nil {
				log.WithError(err).Fatal("Error opening manifest file")
			}

			f = manifest
		}

		report, err := auditor.AuditManifest(rootConfig.manifest, f)
		if err != nil {
			log.WithError(err).Fatal("Error auditing manifest")
		}
		return report
	}

	if k8sinternal.IsRunningInCluster(k8sinternal.DefaultClient) && rootConfig.kubeConfig == "" {
		report, err := auditor.AuditCluster(k8sinternal.ClientOptions{Namespace: rootConfig.namespace, IncludeGenerated: rootConfig.includeGenerated})
		if err != nil {
			log.WithError(err).Fatal("Error auditing cluster")
		}
		return report
	}

	report, err := auditor.AuditLocal(rootConfig.kubeConfig, rootConfig.context, kubeaudit.AuditOptions{Namespace: rootConfig.namespace, IncludeGenerated: rootConfig.includeGenerated})
	if err != nil {
		log.WithError(err).Fatal("Error auditing cluster in local mode")
	}
	return report
}

func initKubeaudit(auditable ...kubeaudit.Auditable) *kubeaudit.Kubeaudit {
	if len(auditable) == 0 {
		allAuditors, err := all.Auditors(config.KubeauditConfig{})
		if err != nil {
			log.WithError(err).Fatal("Error initializing auditors")
		}
		auditable = allAuditors
	}

	auditor, err := kubeaudit.New(auditable)
	if err != nil {
		log.WithError(err).Fatal("Error creating auditor")
	}

	return auditor
}
