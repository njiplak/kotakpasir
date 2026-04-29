package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"nexteam.id/kotakpasir/pkg/kotakpasir"
)

// lsCmd: kp ls
//
// Lists all sandboxes in a compact table. Use `kp inspect <id>` for full JSON.
func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := client().List(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("no sandboxes")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tIMAGE\tSTATE\tNAME")
			for _, sb := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", short(sb.ID), sb.Image, sb.State, sb.Name)
			}
			return tw.Flush()
		},
	}
}

// runCmd: kp run -i alpine:latest
//
// Creates a sandbox and (by default) waits for state=running before printing
// the new sandbox ID. The single-line ID output makes it scriptable:
//
//	ID=$(kp run -i alpine:latest)
//	kp exec "$ID" -- echo hello
func runCmd() *cobra.Command {
	var (
		image, profile, name string
		cpus                 float64
		memMB                int64
		ttl                  time.Duration
		wait                 bool
		waitTimeout          time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Create a sandbox and print its id",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if image == "" && profile == "" {
				return fmt.Errorf("either --image or --profile is required")
			}
			ctx := cmd.Context()
			c := client()

			sb, err := c.Create(ctx, kotakpasir.CreateOptions{
				Name:     name,
				Image:    image,
				Profile:  profile,
				Cpus:     cpus,
				MemoryMB: memMB,
				TTL:      ttl,
			})
			if err != nil {
				return err
			}

			if wait {
				wctx, cancel := context.WithTimeout(ctx, waitTimeout)
				defer cancel()
				sb, err = c.WaitFor(wctx, sb.ID, kotakpasir.StateRunning, 0)
				if err != nil {
					return err
				}
			}
			fmt.Println(sb.ID)
			return nil
		},
	}
	cmd.Flags().StringVarP(&image, "image", "i", "", "OCI image (e.g. alpine:latest)")
	cmd.Flags().StringVarP(&profile, "profile", "p", "", "named profile from kotakpasir.yaml")
	cmd.Flags().StringVar(&name, "name", "", "human-readable name (optional)")
	cmd.Flags().Float64Var(&cpus, "cpus", 0, "CPU limit (0 = policy default)")
	cmd.Flags().Int64Var(&memMB, "memory-mb", 0, "memory in MB (0 = policy default)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "auto-delete after this duration (e.g. 30m)")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for state=running before returning")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Second, "max time to wait for running")
	return cmd
}

// execCmd: kp exec <id> [--stream] -- cmd [args...]
//
// Buffered by default; pass --stream for live SSE output (good for long jobs).
// The CLI exits with the in-sandbox command's exit code so scripts work:
//
//	if ! kp exec "$ID" -- ./run-tests.sh; then ...
func execCmd() *cobra.Command {
	var stream bool
	cmd := &cobra.Command{
		Use:   "exec [flags] <id> -- <cmd> [args...]",
		Short: "Run a command inside a sandbox",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cmdArgs := args[1:]
			ctx := cmd.Context()
			c := client()
			opts := kotakpasir.ExecOptions{Cmd: cmdArgs}

			if stream {
				res, err := c.ExecStream(ctx, id, opts, os.Stdout, os.Stderr)
				if err != nil {
					return err
				}
				exitCode = res.ExitCode
				return nil
			}

			res, err := c.Exec(ctx, id, opts)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(os.Stdout, res.Stdout)
			_, _ = io.WriteString(os.Stderr, res.Stderr)
			exitCode = res.ExitCode
			return nil
		},
	}
	cmd.Flags().BoolVar(&stream, "stream", false, "stream output line-by-line via SSE (use for long-running commands)")
	return cmd
}

// stopCmd: kp stop <id>
//
// Stops the sandbox container without deleting the SQLite row.
// Subsequent `kp exec` will fail with "sandbox is stopped, not running".
func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id>",
		Short: "Stop a running sandbox (does not delete it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Stop(cmd.Context(), args[0])
		},
	}
}

// rmCmd: kp rm <id>
//
// Stops and removes the sandbox + any attached proxy/network. Idempotent
// from the caller's perspective: if the sandbox doesn't exist, prints the
// not-found error.
func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>",
		Aliases: []string{"delete"},
		Short:   "Stop and remove a sandbox (also tears down proxy + network if any)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Delete(cmd.Context(), args[0])
		},
	}
}

// inspectCmd: kp inspect <id>
//
// Pretty-prints the sandbox JSON. Useful for piping into jq.
func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id>",
		Short: "Show full sandbox details as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := client().Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(sb)
		},
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
