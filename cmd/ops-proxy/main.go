package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/prow/pkg/config/secret"
	"sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/interrupts"
	"sigs.k8s.io/prow/pkg/logrusutil"

	"github.com/openshift/ci-tools-standalone/internal/opsproxy"
)

type options struct {
	logLevel              string
	port                  int
	gracePeriod           time.Duration
	kubernetesOptions     flagutil.KubernetesOptions
	slackTokenPath        string
	slackChannel          string
	setChannelTopic       bool
	hookTokenPath         string
	apiTokenPath          string
	ackAllowlist          string
	alertmanagerURL       string
	alertmanagerTokenPath string
	configmapNamespace    string
	configmapName         string
	reconcileInterval     time.Duration
}

func gatherOptions() (options, error) {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.logLevel, "log-level", "info", "Level at which to log output.")
	fs.IntVar(&o.port, "port", 8080, "Port to run the server on")
	o.kubernetesOptions.AddFlags(fs)
	fs.DurationVar(&o.gracePeriod, "gracePeriod", time.Second*10, "Grace period for server shutdown")
	fs.StringVar(&o.slackTokenPath, "slack-token-path", "", "Path to the file containing the Slack bot oauth token.")
	fs.StringVar(&o.slackChannel, "slack-channel", "#dptp-robot-testing", "Slack channel for the incident board and cards.")
	fs.BoolVar(&o.setChannelTopic, "set-channel-topic", true, "If true, set the Slack channel topic to RED N OPEN · names.")
	fs.StringVar(&o.hookTokenPath, "hook-token-path", "", "Path to the bearer token Alertmanager sends on POST /hook/alertmanager.")
	fs.StringVar(&o.apiTokenPath, "api-token-path", "", "Path to the bearer token for in-cluster POST /ack, /unack, /needs-human.")
	fs.StringVar(&o.ackAllowlist, "ack-allowlist", "", "Comma-separated Slack user IDs allowed to ack. Empty deny-all.")
	fs.StringVar(&o.alertmanagerURL, "alertmanager-url", "", "Alertmanager base URL (required). Silences and alerts use /api/v2.")
	fs.StringVar(&o.alertmanagerTokenPath, "alertmanager-token-path", "", "Optional path to a bearer token for the Alertmanager API.")
	fs.StringVar(&o.configmapNamespace, "configmap-namespace", "ci", "Namespace of the ops-proxy ConfigMap.")
	fs.StringVar(&o.configmapName, "configmap-name", "ops-proxy", "Name of the ops-proxy ConfigMap.")
	fs.DurationVar(&o.reconcileInterval, "reconcile-interval", time.Minute, "How often to reconcile from Alertmanager firing alerts and silences.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return o, fmt.Errorf("failed to parse flags: %w", err)
	}
	return o, nil
}

func validateOptions(o options) error {
	_, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid --log-level: %w", err)
	}
	if o.slackTokenPath == "" {
		return fmt.Errorf("--slack-token-path is required")
	}
	if o.hookTokenPath == "" {
		return fmt.Errorf("--hook-token-path is required")
	}
	if o.apiTokenPath == "" {
		return fmt.Errorf("--api-token-path is required")
	}
	if strings.TrimSpace(o.alertmanagerURL) == "" {
		return fmt.Errorf("--alertmanager-url is required")
	}
	return o.kubernetesOptions.Validate(false)
}

func main() {
	logrusutil.ComponentInit()
	o, err := gatherOptions()
	if err != nil {
		logrus.WithError(err).Fatal("failed to gather options")
	}
	if err := validateOptions(o); err != nil {
		logrus.WithError(err).Fatal("invalid options")
	}
	level, _ := logrus.ParseLevel(o.logLevel)
	logrus.SetLevel(level)

	secretPaths := []string{o.slackTokenPath, o.hookTokenPath, o.apiTokenPath}
	if o.alertmanagerTokenPath != "" {
		secretPaths = append(secretPaths, o.alertmanagerTokenPath)
	}
	if err := secret.Add(secretPaths...); err != nil {
		logrus.WithError(err).Fatal("failed to start secrets agent")
	}

	cfg, err := o.kubernetesOptions.InfrastructureClusterConfig(false)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to load kubeconfig")
	}
	kubeClient, err := ctrlruntimeclient.New(cfg, ctrlruntimeclient.Options{})
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create client")
	}

	var amToken func() []byte
	if o.alertmanagerTokenPath != "" {
		path := o.alertmanagerTokenPath
		amToken = func() []byte { return secret.GetSecret(path) }
	}
	am := opsproxy.NewAlertmanagerClient(o.alertmanagerURL, amToken)
	slackPath := o.slackTokenPath
	slackClient := opsproxy.NewSlackAPI(func() []byte { return secret.GetSecret(slackPath) })
	store := opsproxy.NewStore(kubeClient, o.configmapNamespace, o.configmapName)
	hookPath := o.hookTokenPath
	apiPath := o.apiTokenPath
	server := opsproxy.NewServer(
		logrus.WithField("component", "ops-proxy"),
		opsproxy.Config{
			HookToken:    func() []byte { return secret.GetSecret(hookPath) },
			APIToken:     func() []byte { return secret.GetSecret(apiPath) },
			Allowlist:    opsproxy.ParseAllowlist(o.ackAllowlist),
			SlackChannel: o.slackChannel,
			SetTopic:     o.setChannelTopic,
		},
		store,
		am,
		slackClient,
	)

	httpServer := &http.Server{
		Addr:    ":" + strconv.Itoa(o.port),
		Handler: server.Handler(),
	}
	interrupts.ListenAndServe(httpServer, o.gracePeriod)
	logrus.Debug("Server ready.")

	interrupts.TickLiteral(func() {
		if err := server.Reconcile(interrupts.Context()); err != nil {
			logrus.WithError(err).Error("reconcile failed")
		}
	}, o.reconcileInterval)

	interrupts.WaitForGracefulShutdown()
}
