package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/fattyatom/clusterdeck/internal/config"
	"github.com/fattyatom/clusterdeck/internal/kube"
	"github.com/fattyatom/clusterdeck/internal/metrics"
	"github.com/fattyatom/clusterdeck/internal/server"
	"github.com/fattyatom/clusterdeck/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.FromOS()
	if err != nil {
		return err
	}
	restCfg, err := restConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	core, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	cluster, err := kube.New(core, dyn, kube.Options{
		HideNamespaces:          cfg.HideNamespaces,
		BackupSelector:          cfg.BackupSelector,
		RestoreCheckSelector:    cfg.RestoreCheckSelector,
		RestoreTemplateSelector: cfg.RestoreTemplateSelector,
	})
	if err != nil {
		return err
	}

	var provider metrics.Provider = metrics.None{}
	switch cfg.MetricsProvider {
	case "signoz":
		provider = &metrics.SigNoz{URL: cfg.SigNozURL, APIKey: cfg.SigNozAPIKey, Filter: cfg.SigNozFilter}
	case "prometheus":
		provider = &metrics.Prometheus{URL: cfg.PrometheusURL}
	}

	handler := server.New(cluster, provider, server.Options{
		ClusterName:    cfg.ClusterName,
		AuthHeader:     cfg.AuthHeader,
		AllowedUsers:   cfg.AllowedUsers,
		ActionsEnabled: cfg.ActionsEnabled,
		AutoRollback:   cfg.AutoRollback,
		Static:         web.FS(),
		Logger:         logger,
	})
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if cfg.AutoRollback {
		go autoRollback(ctx, cluster, cfg.ActionsNamespaces, cfg.AutoRollbackInterval, logger)
	}
	logger.Info("listening", "addr", cfg.ListenAddr, "metrics", provider.Name(), "actions", cfg.ActionsEnabled)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func autoRollback(ctx context.Context, cluster *kube.Client, namespaces []string, every time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		results, err := cluster.AutoRollback(ctx, namespaces)
		for _, r := range results {
			logger.Warn("auto rollback", "user", "clusterdeck", "namespace", r.Namespace, "deployment", r.Name, "from", r.From, "to", r.To, "reason", r.Reason)
		}
		if err != nil {
			logger.Error("auto rollback check failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
