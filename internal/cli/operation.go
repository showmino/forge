package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newOpCmd(f *Factory) *cobra.Command {
	cmd := group("op", "Watch and cancel long-running operations", "ops", "operation", "operations")
	cmd.Long = `Long-running work, such as creating an environment, runs as an operation on
the Forge API. Commands that start one watch it until it ends; with
--wait=false they print its id instead, and 'forge op watch <id>' picks it up
again. Operations live in the API's memory for 24 hours.`
	cmd.AddCommand(newOpListCmd(f), newOpWatchCmd(f), newOpCancelCmd(f))
	return cmd
}

func newOpWatchCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "watch <id>",
		Short: "Stream an operation's progress until it ends",
		Long: `Stream an operation's progress until it ends: first everything that already
happened, then new events. Exits 0 when it succeeded and 7 when it failed or
was cancelled. With --timeout, stops watching (exit 8) but leaves the
operation running.`,
		Example: `  forge op watch op_9f2c4a1b0c3d
  forge op watch op_9f2c4a1b0c3d --timeout 5m`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name := opName(args[0])
			if !validOpID(name) {
				return exitcode.Usagef("%q is not an operation id (op_<id>)", args[0]).WithHint("see 'forge op list'")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			op, err := f.watchOperation(ctx, c, name)
			if err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return err
			}
			return f.printOperation(format, op)
		},
	}
}

// opName accepts "op_x" or "operations/op_x" and returns "operations/op_x".
func opName(s string) string { return "operations/" + strings.TrimPrefix(s, "operations/") }

// opID returns the id of "operations/op_x": "op_x".
func opID(name string) string { return strings.TrimPrefix(name, "operations/") }

// validOpID checks the shape the API accepts, to fail fast with exit 2.
func validOpID(name string) bool {
	id := opID(name)
	if len(id) != len("op_")+12 || !strings.HasPrefix(id, "op_") {
		return false
	}
	return strings.Trim(id[3:], "0123456789abcdef") == ""
}

// opState reads running, succeeded, failed or cancelled.
func opState(op *forgev1.Operation) string {
	switch {
	case !op.GetDone():
		return "running"
	case op.GetError() == nil:
		return "succeeded"
	case op.GetError().GetCode() == "cancelled":
		return "cancelled"
	default:
		return "failed"
	}
}

// opFailure is nil for a successful operation and exit 7 otherwise.
func opFailure(op *forgev1.Operation) error {
	e := op.GetError()
	if e == nil {
		return nil
	}
	if e.GetCode() == "cancelled" {
		return exitcode.New(exitcode.OperationFailed, "%s was cancelled", op.GetName())
	}
	return exitcode.New(exitcode.OperationFailed, "%s failed: %s (%s)", op.GetName(), e.GetMessage(), e.GetCode())
}

// printOperation prints a finished operation's result.
func (f *Factory) printOperation(format output.Format, op *forgev1.Operation) error {
	if format == output.Table {
		_, err := fmt.Fprintf(f.IO.Out, "%s %s\n", op.GetName(), opState(op))
		return err
	}
	return output.Print(f.IO.Out, format, output.View{Names: []string{op.GetName()}, Data: op})
}

// event prints one progress line on stderr.
func (f *Factory) event(ev *forgev1.OperationEvent) {
	f.infof("  %-10s %s", ev.GetStep(), ev.GetMessage())
}

// watchOperation streams name's events to stderr and returns the finished
// operation. A dropped stream, or a watch the API ended because it fell
// behind, is watched again; the replay's events already printed are skipped.
func (f *Factory) watchOperation(ctx context.Context, c *client.Client, name string) (*forgev1.Operation, error) {
	printed := 0
	// attempt counts consecutive drops without new events, so a long watch
	// survives any number of cut streams as long as it keeps progressing.
	attempt := 0
	for ; ; attempt++ {
		stream, err := c.Operations.WatchOperation(ctx, connect.NewRequest(&forgev1.WatchOperationRequest{Name: name}))
		if err != nil {
			return nil, f.watchErr(ctx, name, err)
		}
		seen, before := 0, printed
		var final *forgev1.Operation
		for stream.Receive() {
			m := stream.Msg()
			if ev := m.GetEvent(); ev != nil {
				if seen >= printed {
					f.event(ev)
					printed++
				}
				seen++
				continue
			}
			final = m.GetOperation()
		}
		err = stream.Err()
		_ = stream.Close()
		if final != nil {
			return final, nil
		}
		again := err == nil || !connect.IsWireError(err) || connect.CodeOf(err) == connect.CodeResourceExhausted
		if printed > before {
			attempt = 0
		}
		if again && attempt < 5 && ctx.Err() == nil {
			if attempt > 0 {
				select {
				case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
				case <-ctx.Done():
					return nil, f.watchErr(ctx, name, ctx.Err())
				}
			}
			continue
		}
		if err == nil {
			err = errors.New("the Forge API ended the watch without a result")
		}
		return nil, f.watchErr(ctx, name, err)
	}
}

// watchErr turns the end of --timeout into exit 8 with the resume hint.
func (f *Factory) watchErr(ctx context.Context, name string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
		return exitcode.New(exitcode.Timeout, "%s is still running", name).
			WithHint("resume with: forge op watch " + opID(name))
	}
	return err
}

func newOpListCmd(f *Factory) *cobra.Command {
	var target string
	var running bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List operations, newest first",
		Example: `  forge op list
  forge op list --running
  forge op list --target environments/pr-142 -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Operations.ListOperations(ctx, connect.NewRequest(&forgev1.ListOperationsRequest{Target: target, Running: running}))
			if err != nil {
				return err
			}
			ops := resp.Msg.GetOperations()
			if len(ops) == 0 && format == output.Table {
				f.infof("No operations.")
				return nil
			}
			v := output.View{Headers: []string{"id", "kind", "target", "state", "age"}, Data: resp.Msg}
			if len(ops) == 0 {
				v.Data = map[string]any{"operations": []any{}} // keep '.operations[]' working in jq
			}
			now := f.Now()
			for _, op := range ops {
				v.Rows = append(v.Rows, []string{opID(op.GetName()), op.GetKind(), op.GetTarget(), opState(op), age(now.Sub(op.GetCreateTime().AsTime()))})
				v.Names = append(v.Names, op.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "only operations on this resource, for example environments/pr-142")
	cmd.Flags().BoolVar(&running, "running", false, "only operations that are still running")
	return cmd
}

func newOpCancelCmd(f *Factory) *cobra.Command {
	var yes, wait bool
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Stop a running operation",
		Long: `Ask a running operation to stop. Work it already did is undone where the
operation's kind allows it (a cancelled environment create removes the
environment). Cancelling a finished operation changes nothing.`,
		Example: `  forge op cancel op_9f2c4a1b0c3d --yes
  forge op cancel op_9f2c4a1b0c3d --yes --wait`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name := opName(args[0])
			if !validOpID(name) {
				return exitcode.Usagef("%q is not an operation id (op_<id>)", args[0]).WithHint("see 'forge op list'")
			}
			if err := f.confirmDestructive("cancel", name, yes); err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Operations.CancelOperation(ctx, connect.NewRequest(&forgev1.CancelOperationRequest{Name: name}))
			if err != nil {
				return err
			}
			op := resp.Msg.GetOperation()
			if op.GetDone() {
				if format != output.Table {
					return f.printOperation(format, op)
				}
				_, err := fmt.Fprintf(f.IO.Out, "%s already finished (%s)\n", name, opState(op))
				return err
			}
			if !wait {
				if format != output.Table {
					return f.printOperation(format, op)
				}
				_, err := fmt.Fprintf(f.IO.Out, "%s cancelling\n", name)
				return err
			}
			if op, err = f.watchOperation(ctx, c, name); err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return err
			}
			return f.printOperation(format, op)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&wait, "wait", false, "watch the operation until it has stopped")
	return cmd
}

// age reads a duration coarsely: <1m, 5m, 3h, 2d.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
