package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"nexteam.id/kotakpasir/pkg/kotakpasir"
)

// logsCmd: kp logs <id> [-f] [--tail N]
//
// Replays captured exec output (stdout + stderr merged in order) from the
// kpd ring buffer. With -f, prints the snapshot then keeps streaming new
// chunks until Ctrl-C. The buffer is bounded (default 256KB per sandbox);
// older output is evicted FIFO.
func logsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
	)
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show captured exec output for a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			ctx := cmd.Context()
			c := client()
			opts := kotakpasir.LogsOptions{TailLines: tail}

			if follow {
				return c.LogsStream(ctx, id, opts, os.Stdout, os.Stderr)
			}
			s, err := c.Logs(ctx, id, opts)
			if err != nil {
				return err
			}
			_, _ = os.Stdout.WriteString(s)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new output as it arrives")
	cmd.Flags().IntVar(&tail, "tail", 0, "show only the last N lines (0 = all buffered)")
	return cmd
}

// watchCmd: kp watch [id] [--interval Ns]
//
// Polls kpd and prints state changes. Without an id, watches every sandbox.
// Pure client-side; no server-side change feed required. Press Ctrl-C to stop.
func watchCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "watch [id]",
		Short: "Stream state changes for one or all sandboxes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c := client()

			if len(args) == 1 {
				return watchOne(ctx, c, args[0], interval)
			}
			return watchAll(ctx, c, interval)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "poll interval")
	return cmd
}

func watchOne(ctx context.Context, c *kotakpasir.Client, id string, interval time.Duration) error {
	var prev string
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		sb, err := c.Get(ctx, id)
		if err != nil {
			return err
		}
		if string(sb.State) != prev {
			fmt.Printf("%s  %s  %s\n", time.Now().Format("15:04:05"), short(sb.ID), sb.State)
			prev = string(sb.State)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func watchAll(ctx context.Context, c *kotakpasir.Client, interval time.Duration) error {
	prev := map[string]string{}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	for {
		list, err := c.List(ctx)
		if err != nil {
			return err
		}
		seen := map[string]struct{}{}
		// Stable order so output is reproducible across polls.
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

		for _, sb := range list {
			seen[sb.ID] = struct{}{}
			if prev[sb.ID] != string(sb.State) {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					time.Now().Format("15:04:05"), short(sb.ID), sb.State, sb.Image)
				prev[sb.ID] = string(sb.State)
			}
		}
		// Detect deletions: sandbox in prev but not in seen.
		for id, st := range prev {
			if _, ok := seen[id]; ok {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t(was %s)\n",
				time.Now().Format("15:04:05"), short(id), "removed", st)
			delete(prev, id)
		}
		_ = tw.Flush()

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// completionCmd: kp completion {bash|zsh|fish|powershell}
//
// Emits a shell completion script to stdout. Shells consume it via:
//
//	source <(kp completion bash)
//	kp completion zsh > "${fpath[1]}/_kp"
func completionCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:                   "completion {bash|zsh|fish|powershell}",
		Short:                 "Generate shell completion script",
		Long:                  completionLong,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			}
			return fmt.Errorf("unsupported shell %q", args[0])
		},
	}
}

const completionLong = `Generate shell completion scripts for kp.

Bash:
  source <(kp completion bash)
  # Permanent: kp completion bash > /etc/bash_completion.d/kp

Zsh:
  # Once per machine (zsh-completions on $fpath):
  kp completion zsh > "${fpath[1]}/_kp"
  compinit

Fish:
  kp completion fish | source

PowerShell:
  kp completion powershell | Out-String | Invoke-Expression
`

