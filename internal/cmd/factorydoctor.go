package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/smeltdev/smelt/internal/config"
	"github.com/smeltdev/smelt/internal/iostream"
	"github.com/smeltdev/smelt/internal/sidecar/docker"
	"github.com/spf13/cobra"
)

func newFactoryDoctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Inspect Docker backing storage, free space and factory cache caps", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := sessionProjectRoot(cmd.Context())
			if err != nil {
				return err
			}
			cfg, err := config.LoadProjectConfig(root)
			if err != nil {
				return err
			}
			provider, err := docker.New()
			if err != nil {
				return dockerUnreachable(err)
			}
			defer func() { _ = provider.Close() }()
			provider.CacheKey = config.ProjectHash(root)
			provider.ProjectPath = root
			provider.MinFreeBytes = cfg.Factory.MinFreeBytes()
			provider.CacheMaxBytes = cfg.Factory.CacheMaxBytes()
			report, reportErr := provider.ReportStorage(cmd.Context())
			printStorageReport(iostream.FromCmd(cmd).Out, report, reportErr)
			if reportErr != nil {
				return &userError{msg: "Docker storage diagnostics failed.", err: reportErr}
			}
			return nil
		}}
}

func printStorageReport(w io.Writer, report docker.StorageReport, err error) {
	_, _ = fmt.Fprintf(w, "Docker endpoint: %s\nDaemon: %s; root: %s\n", report.Host.Endpoint, report.Host.DaemonID, report.Host.DockerRoot)
	if report.Host.Settings != "" {
		_, _ = fmt.Fprintf(w, "Desktop settings: %s\n", report.Host.Settings)
	}
	if report.Host.BackingImage != "" {
		_, _ = fmt.Fprintf(w, "Backing image: %s\n", report.Host.BackingImage)
	}
	if report.Host.FilesystemPath != "" {
		_, _ = fmt.Fprintf(w, "Backing filesystem: %s\n", report.Host.FilesystemPath)
	}
	for _, fs := range report.Filesystems {
		_, _ = fmt.Fprintf(w, "%s (%s): %.2f GB free\n", fs.Name, fs.Path, float64(fs.Free)/1e9)
	}
	for _, cache := range report.Caches {
		measurement := cache.Unavailable
		size := "unavailable"
		if cache.MeasuredAt != "" {
			size = fmt.Sprintf("%.2f GB", float64(cache.Bytes)/1e9)
			at, parseErr := time.Parse(time.RFC3339, cache.MeasuredAt)
			if parseErr == nil {
				measurement = fmt.Sprintf("measured %s ago", time.Since(at).Round(time.Second))
			}
		}
		_, _ = fmt.Fprintf(w, "%s: %s / %.2f GB cap (%s)\n", cache.Name, size, float64(cache.Cap)/1e9, measurement)
	}
	if err != nil {
		_, _ = fmt.Fprintf(w, "Storage unavailable: %v\n", err)
	}
}
