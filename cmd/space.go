package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/gordonbeeming/shunt/internal/storage"
	"github.com/spf13/cobra"
)

var (
	spaceCollect  = storage.Collect
	spaceLoadApps = spaceApps
)

func newSpaceCmd() *cobra.Command {
	var all, asJSON bool
	command := &cobra.Command{
		Use:   "space",
		Short: "Explain Shunt disk usage without treating APFS clone totals as reclaimable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			apps, err := spaceLoadApps(all)
			if err != nil {
				return err
			}
			report, err := spaceCollect(cmd.Context(), apps)
			if err != nil {
				return err
			}
			if err := storage.ValidateReport(report); err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(report)
			}
			return printSpace(cmd.OutOrStdout(), report)
		},
	}
	command.Flags().BoolVarP(&all, "all", "a", false, "report every registered project")
	command.Flags().BoolVar(&asJSON, "json", false, "machine-readable output with explicit measurement semantics")
	return command
}

func spaceApps(all bool) ([]state.App, error) {
	if !all {
		app, _, err := loadCurrentApp()
		if err != nil {
			return nil, err
		}
		return []state.App{app}, nil
	}
	registry, err := state.LoadRegistry()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(registry.Projects))
	for name := range registry.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	apps := make([]state.App, 0, len(names))
	for _, name := range names {
		app, err := state.LoadApp(registry.Projects[name])
		if err != nil {
			return nil, fmt.Errorf("load %s for space report: %w", name, err)
		}
		apps = append(apps, app)
	}
	return apps, nil
}

func printSpace(out io.Writer, report storage.Report) error {
	for _, project := range report.Projects {
		fmt.Fprintf(out, "%s\n", project.Name)
		if project.Filesystem.Observation == "observed" {
			fmt.Fprintf(out, "  filesystem (physical): %s used of %s; %s available\n",
				formatBytes(project.Filesystem.UsedBytes), formatBytes(project.Filesystem.TotalBytes), formatBytes(project.Filesystem.AvailableBytes))
		} else {
			fmt.Fprintf(out, "  filesystem (physical): unavailable (%s)\n", project.Filesystem.Detail)
		}
		fmt.Fprintf(out, "  project scan (logical, clone-shared; not reclaimable): %s\n", formatMeasurement(project.Logical))
		fmt.Fprintf(out, "  registered source (legacy checkout reference only; never a runtime host): %s (%s logical)\n",
			project.Source.Measurement.Path, formatMeasurement(project.Source.Measurement))
		printGitEvidence(out, "source", project.Source.Git)
		for _, managed := range project.Managed {
			protection := "managed"
			if managed.Protected {
				protection = "managed/protected"
			}
			fmt.Fprintf(out, "  %s %s (logical; not reclaimable): %s\n", protection, managed.Name, formatMeasurement(managed))
		}
		if project.GitArchives.Observation == "observed" {
			fmt.Fprintf(out, "  managed Git refs: %d recovery, %d preservation witnesses\n", project.GitArchives.RecoveryRefs, project.GitArchives.WitnessRefs)
		}
		for _, unknown := range project.Unclassified {
			fmt.Fprintf(out, "  unclassified %s (logical; ownership/reclaimability unverified): %s\n", unknown.Name, formatMeasurement(unknown))
		}
		writer := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(writer, "  SIDING\tSOURCE\tGENERATED*\tOUT\tDATA*")
		for _, siding := range project.Sidings {
			fmt.Fprintf(writer, "  %s\t%s\t%s\t%s\t%s\n", siding.Name,
				formatMeasurement(siding.Source.Measurement), formatMeasurement(siding.Generated),
				formatMeasurement(siding.Output), formatMeasurement(siding.Data))
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(out, "  * logical values overlap source/project scans and may share APFS blocks; they are not reclaimable estimates")
		for _, siding := range project.Sidings {
			printGitEvidence(out, siding.Name, siding.Source.Git)
		}
		for _, baseline := range project.Baselines {
			fmt.Fprintf(out, "  protected %s baseline (logical/shared): %s\n", baseline.Name, formatMeasurement(baseline))
		}
	}
	if report.Container.Observation != "observed" {
		fmt.Fprintf(out, "Apple container disk usage: unavailable (%s); this command did not start the service\n", report.Container.Detail)
		return nil
	}
	fmt.Fprintln(out, "Apple container disk usage (official; the only reclaimable figures in this report):")
	var indented bytes.Buffer
	if err := json.Indent(&indented, report.Container.Data, "  ", "  "); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, indented.String())
	return err
}

func formatMeasurement(measurement storage.Measurement) string {
	if measurement.Observation == "observed" {
		return formatBytes(measurement.LogicalBytes)
	}
	if measurement.Detail != "" {
		return fmt.Sprintf("%s (%s)", measurement.Observation, measurement.Detail)
	}
	return measurement.Observation
}

func printGitEvidence(out io.Writer, label string, evidence storage.GitEvidence) {
	if evidence.Observation != "observed" {
		fmt.Fprintf(out, "  git %-12s %s", label+":", evidence.Observation)
		if evidence.Detail != "" {
			fmt.Fprintf(out, " (%s)", evidence.Detail)
		}
		fmt.Fprintln(out)
		return
	}
	head := evidence.Head
	if len(head) > 12 {
		head = head[:12]
	}
	upstream := evidence.Upstream
	if upstream == "" {
		upstream = "(none)"
	}
	dirty := "clean"
	if evidence.Dirty {
		dirty = fmt.Sprintf("dirty, %d untracked", evidence.Untracked)
	}
	last := evidence.LastCommit
	if last == "" {
		last = "(unknown)"
	}
	fmt.Fprintf(out, "  git %-12s branch %s; HEAD %s; upstream %s (+%d/-%d); %s; unique %d; last %s\n",
		label+":", evidence.Branch, head, upstream, evidence.Ahead, evidence.Behind, dirty, evidence.UniqueCommits, last)
	if evidence.Preservation != nil {
		status := "protected"
		if evidence.Preservation.Preserved {
			status = "preserved"
		}
		fmt.Fprintf(out, "    committed work: %s (%s)\n", status, evidence.Preservation.Reason)
	}
}

func formatBytes(bytes int64) string {
	const unit = int64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, suffix := range units {
		value /= float64(unit)
		if value < float64(unit) || suffix == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}
