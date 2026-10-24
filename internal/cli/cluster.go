package cli

import (
	"cmp"
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
			v := output.View{Headers: []string{"name", "status", "cpu", "memory", "disk", "nodes", "kubernetes"}, Data: resp.Msg}
			for _, cl := range resp.Msg.GetClusters() {
				t := cl.GetTotals()
				m, s := cl.GetMetricsAvailable(), cl.GetStorageAvailable()
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(cl.GetName(), "clusters/"), clusterStatus(cl),
					usedPct(m, t.GetCpuUsedMillis(), t.GetCpuMillis()), usedPct(m, t.GetMemoryUsedBytes(), t.GetMemoryBytes()),
					usedPct(s, t.GetDiskUsedBytes(), t.GetDiskBytes()),
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
		Short: "Show a cluster, its nodes and its volumes",
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
	m, s := cl.GetMetricsAvailable(), cl.GetStorageAvailable()
	disk := "-"
	if s && t.GetDiskBytes() > 0 {
		disk = usedOf(true, t.GetDiskUsedBytes(), t.GetDiskBytes(), gibibytes)
	}
	rows := [][]string{
		{"name", cl.GetName()},
		{"status", clusterStatus(cl)},
		{"kubernetes", cl.GetKubernetesVersion()},
		{"cpu", usedOf(m, t.GetCpuUsedMillis(), t.GetCpuMillis(), cores)},
		{"memory", usedOf(m, t.GetMemoryUsedBytes(), t.GetMemoryBytes(), gibibytes)},
		{"disk", disk},
		{"pods", fmt.Sprintf("%d of %d", t.GetPods(), t.GetPodsMax())},
	}
	if cl.GetGrafanaUrl() != "" {
		rows = append(rows, []string{"grafana", cl.GetGrafanaUrl()})
	}
	if err := output.Print(f.IO.Out, output.Table, output.View{Headers: []string{"field", "value"}, Rows: rows}); err != nil {
		return err
	}
	nodes := output.View{Headers: []string{"node", "roles", "condition", "schedulable", "cpu", "memory", "disk", "pods"}}
	for _, n := range cl.GetNodes() {
		c := n.GetCapacity()
		sched := "yes"
		if !n.GetSchedulable() {
			sched = "no"
		}
		disk := "-"
		if s && c.GetDiskBytes() > 0 {
			disk = usedOf(true, c.GetDiskUsedBytes(), c.GetDiskBytes(), gibibytes)
		}
		nodes.Rows = append(nodes.Rows, []string{
			n.GetName(), strings.Join(n.GetRoles(), ","), n.GetCondition(), sched,
			usedOf(m, c.GetCpuUsedMillis(), c.GetCpuMillis(), cores), usedOf(m, c.GetMemoryUsedBytes(), c.GetMemoryBytes(), gibibytes),
			disk, fmt.Sprintf("%d/%d", c.GetPods(), c.GetPodsMax()),
		})
	}
	if _, err := fmt.Fprintln(f.IO.Out); err != nil {
		return err
	}
	if err := output.Print(f.IO.Out, output.Table, nodes); err != nil {
		return err
	}
	if vols := cl.GetVolumes(); len(vols) > 0 {
		view := output.View{Headers: []string{"volume", "service", "env", "used"}}
		for _, v := range vols {
			svc := strings.TrimPrefix(v.GetService(), "services/")
			view.Rows = append(view.Rows, []string{
				v.GetNamespace() + "/" + v.GetClaim(), cmp.Or(svc, "-"), cmp.Or(v.GetEnvironment(), "-"),
				usedOf(true, v.GetUsedBytes(), v.GetCapacityBytes(), gibibytes),
			})
		}
		if _, err := fmt.Fprintln(f.IO.Out); err != nil {
			return err
		}
		if err := output.Print(f.IO.Out, output.Table, view); err != nil {
			return err
		}
	}
	if !m {
		f.infof("No usage figures: metrics-server did not answer the Forge API.")
	}
	if !s {
		f.infof("No disk or volume figures: the Forge API has no Prometheus (--prometheus-url), or it did not answer.")
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
// condition, then the fullest of CPU, memory, disk or a volume from 80% used,
// else Healthy.
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
	worst, what := 0, ""
	consider := func(p int, w string) {
		if p > worst {
			worst, what = p, w
		}
	}
	t := cl.GetTotals()
	if cl.GetMetricsAvailable() {
		consider(pct(t.GetMemoryUsedBytes(), t.GetMemoryBytes()), "Memory")
		consider(pct(t.GetCpuUsedMillis(), t.GetCpuMillis()), "CPU")
	}
	if cl.GetStorageAvailable() {
		consider(pct(t.GetDiskUsedBytes(), t.GetDiskBytes()), "Disk")
		for _, v := range cl.GetVolumes() {
			consider(pct(v.GetUsedBytes(), v.GetCapacityBytes()), "Volume "+v.GetNamespace()+"/"+v.GetClaim())
		}
	}
	if worst >= warnAt {
		return fmt.Sprintf("%s at %d%%", what, worst)
	}
	return "Healthy"
}

func pct(used, total int64) int {
	if total <= 0 {
		return 0
	}
	return int((used*100 + total/2) / total)
}

// usedPct is "42%", or "-" without figures.
func usedPct(ok bool, used, total int64) string {
	if !ok || total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", pct(used, total))
}

// usedOf is "42% of 8 cores", or "8 cores" without figures.
func usedOf(ok bool, used, total int64, unit func(int64) string) string {
	if !ok || total <= 0 {
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
