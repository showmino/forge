package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/output"
)

func newClusterCmd(f *Factory) *cobra.Command {
	cmd := group("cluster", "Show how much room the cluster has left (admins)", "clusters")
	cmd.Long = `The Kubernetes cluster Forge runs on: its nodes, their conditions and how much
of their CPU and memory is in use, read by the Forge API from Kubernetes and
metrics-server. Only admins may see it.`
	cmd.AddCommand(newClusterListCmd(f), newClusterGetCmd(f))
	return cmd
}

func newClusterListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the clusters with their status and use",
		Long: `List the clusters with their status and how much of their CPU and memory is
in use. The figures are over the nodes that take new pods.`,
		Example: `  forge cluster list
  forge cluster list -o json | jq '.clusters[].totals'`,
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
			resp, err := c.Clusters.ListClusters(ctx, connect.NewRequest(&forgev1.ListClustersRequest{}))
			if err != nil {
				return err
			}
			v := output.View{Headers: []string{"name", "status", "cpu", "memory", "nodes", "kubernetes"}, Data: resp.Msg}
			for _, cl := range resp.Msg.GetClusters() {
				t := cl.GetTotals()
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(cl.GetName(), "clusters/"), clusterStatus(cl),
					usedPct(cl, t.GetCpuUsedMillis(), t.GetCpuMillis()), usedPct(cl, t.GetMemoryUsedBytes(), t.GetMemoryBytes()),
					fmt.Sprint(len(cl.GetNodes())), cl.GetKubernetesVersion(),
				})
				v.Names = append(v.Names, cl.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

func newClusterGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a cluster and its nodes",
		Example: `  forge cluster get default
  forge cluster get default -o json | jq '.nodes[] | select(.condition != "Ready")'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			name := "clusters/" + strings.TrimPrefix(args[0], "clusters/")
			resp, err := c.Clusters.GetCluster(ctx, connect.NewRequest(&forgev1.GetClusterRequest{Name: name}))
			if err != nil {
				return err
			}
			cl := resp.Msg.GetCluster()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{cl.GetName()}, Data: cl})
			}
			return f.printCluster(cl)
		},
	}
}

func (f *Factory) printCluster(cl *forgev1.Cluster) error {
	t := cl.GetTotals()
	rows := [][]string{
		{"name", cl.GetName()},
		{"status", clusterStatus(cl)},
		{"kubernetes", cl.GetKubernetesVersion()},
		{"cpu", usedOf(cl, t.GetCpuUsedMillis(), t.GetCpuMillis(), cores)},
		{"memory", usedOf(cl, t.GetMemoryUsedBytes(), t.GetMemoryBytes(), gibibytes)},
		{"pods", fmt.Sprintf("%d of %d", t.GetPods(), t.GetPodsMax())},
	}
	if cl.GetGrafanaUrl() != "" {
		rows = append(rows, []string{"grafana", cl.GetGrafanaUrl()})
	}
	if err := output.Print(f.IO.Out, output.Table, output.View{Headers: []string{"field", "value"}, Rows: rows}); err != nil {
		return err
	}
	nodes := output.View{Headers: []string{"node", "roles", "condition", "schedulable", "cpu", "memory", "pods"}}
	for _, n := range cl.GetNodes() {
		c := n.GetCapacity()
		sched := "yes"
		if !n.GetSchedulable() {
			sched = "no"
		}
		nodes.Rows = append(nodes.Rows, []string{
			n.GetName(), strings.Join(n.GetRoles(), ","), n.GetCondition(), sched,
			usedOf(cl, c.GetCpuUsedMillis(), c.GetCpuMillis(), cores), usedOf(cl, c.GetMemoryUsedBytes(), c.GetMemoryBytes(), gibibytes),
			fmt.Sprintf("%d/%d", c.GetPods(), c.GetPodsMax()),
		})
	}
	if _, err := fmt.Fprintln(f.IO.Out); err != nil {
		return err
	}
	if err := output.Print(f.IO.Out, output.Table, nodes); err != nil {
		return err
	}
	if !cl.GetMetricsAvailable() {
		f.infof("No usage figures: metrics-server did not answer the Forge API.")
	}
	return nil
}

// Thresholds of the dashboard: amber from warnAt percent used.
const warnAt = 80

// conditionWords says a node condition in a status line, worst first.
var conditionWords = []struct{ condition, words string }{
	{"NotReady", "not ready"},
	{"MemoryPressure", "memory pressure"},
	{"DiskPressure", "disk pressure"},
	{"PIDPressure", "PID pressure"},
	{"NetworkUnavailable", "network unavailable"},
}

// clusterStatus is the cluster's worst problem in a few words: a node
// condition, then CPU or memory from 80% used, else Healthy.
func clusterStatus(cl *forgev1.Cluster) string {
	for _, cw := range conditionWords {
		n := 0
		for _, node := range cl.GetNodes() {
			if node.GetCondition() == cw.condition {
				n++
			}
		}
		if n == 0 {
			continue
		}
		if cw.condition == "NotReady" {
			return plural(n, "node") + " not ready"
		}
		return strings.ToUpper(cw.words[:1]) + cw.words[1:] + " on " + plural(n, "node")
	}
	if cl.GetMetricsAvailable() {
		t := cl.GetTotals()
		cpu, mem := pct(t.GetCpuUsedMillis(), t.GetCpuMillis()), pct(t.GetMemoryUsedBytes(), t.GetMemoryBytes())
		if mem >= warnAt && mem >= cpu {
			return fmt.Sprintf("Memory at %d%%", mem)
		}
		if cpu >= warnAt {
			return fmt.Sprintf("CPU at %d%%", cpu)
		}
	}
	return "Healthy"
}

func pct(used, total int64) int {
	if total <= 0 {
		return 0
	}
	return int((used*100 + total/2) / total)
}

// usedPct is "42%", or "-" without metrics.
func usedPct(cl *forgev1.Cluster, used, total int64) string {
	if !cl.GetMetricsAvailable() || total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", pct(used, total))
}

// usedOf is "42% of 8 cores", or "8 cores" without metrics.
func usedOf(cl *forgev1.Cluster, used, total int64, unit func(int64) string) string {
	if !cl.GetMetricsAvailable() || total <= 0 {
		return unit(total)
	}
	return fmt.Sprintf("%d%% of %s", pct(used, total), unit(total))
}

func cores(millis int64) string {
	if millis%1000 == 0 {
		return plural(int(millis/1000), "core")
	}
	return fmt.Sprintf("%.1f cores", float64(millis)/1000)
}

func gibibytes(b int64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}
