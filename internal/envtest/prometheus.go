package envtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

// promSeries is one series of an instant vector, as Prometheus answers it.
type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

func series(value string, labels ...string) promSeries {
	m := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		m[labels[i]] = labels[i+1]
	}
	return promSeries{Metric: m, Value: [2]any{1761300000.0, value}}
}

// Prometheus is a Prometheus with the shapes of the owner's cluster (k3s,
// kube-prometheus-stack) for the nodes of Nodes:
//   - srv-kube's root disk names its node; srv-cron's names only its IP;
//   - payments-production/data is reported once per API server, under
//     exported_namespace, as k3s does; monitoring/prometheus-db plainly.
//
// Disks: srv-kube 100 GiB with 25 free, srv-cron 200 GiB with 180 free,
// srv-lab 50 GiB with 5 free. Volumes: data 9 of 10 GiB, prometheus-db 20 of 50.
func Prometheus() *httptest.Server {
	answers := map[string][]promSeries{
		`node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|ramfs|overlay|squashfs"}`: {
			series("107374182400", "kubernetes_node", "srv-kube", "instance", "10.150.0.10:9100", "mountpoint", "/"),
			series("214748364800", "instance", "10.150.0.15:9100", "mountpoint", "/"),
			series("53687091200", "kubernetes_node", "srv-lab", "instance", "10.150.0.13:9100", "mountpoint", "/"),
		},
		`node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|ramfs|overlay|squashfs"}`: {
			series("26843545600", "kubernetes_node", "srv-kube", "instance", "10.150.0.10:9100"),
			series("193273528320", "instance", "10.150.0.15:9100"),
			series("5368709120", "kubernetes_node", "srv-lab", "instance", "10.150.0.13:9100"),
		},
		`kubelet_volume_stats_capacity_bytes`: {
			series("10737418240", "exported_namespace", "payments-production", "namespace", "default", "persistentvolumeclaim", "data", "instance", "10.150.0.10:6443"),
			series("10737418240", "exported_namespace", "payments-production", "namespace", "default", "persistentvolumeclaim", "data", "instance", "10.150.0.11:6443"),
			series("53687091200", "namespace", "monitoring", "persistentvolumeclaim", "prometheus-db"),
		},
		`kubelet_volume_stats_used_bytes`: {
			series("9663676416", "exported_namespace", "payments-production", "namespace", "default", "persistentvolumeclaim", "data", "instance", "10.150.0.10:6443"),
			series("9663676416", "exported_namespace", "payments-production", "namespace", "default", "persistentvolumeclaim", "data", "instance", "10.150.0.11:6443"),
			series("21474836480", "namespace", "monitoring", "persistentvolumeclaim", "prometheus-db"),
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		result, ok := answers[r.URL.Query().Get("query")]
		if !ok {
			result = []promSeries{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
}
