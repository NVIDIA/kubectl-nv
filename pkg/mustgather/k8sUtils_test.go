package mustgather

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// TestIsOpenShiftClusterOnAnUnreachableCluster covers the case the gatherer
// meets first on a plain Kubernetes cluster: the OpenShift endpoint does not
// answer. It used to call os.Exit on a configuration it could not build, which
// took the whole must-gather down with it.
func TestIsOpenShiftClusterOnAnUnreachableCluster(t *testing.T) {
	version, isOpenShift := isOpenShiftCluster(&rest.Config{Host: "https://127.0.0.1:1"})
	if isOpenShift {
		t.Errorf("isOpenShift = true for an unreachable cluster, version %q", version)
	}
	if version != "" {
		t.Errorf("version = %q, want empty", version)
	}
}

// TestExecuteRemoteCommandUsesTheGivenConfig pins the exec at the cluster the
// caller named. It used to reload the default kubeconfig, which sent the exec
// to whatever cluster the current context pointed at rather than the one
// --kubeconfig named.
func TestExecuteRemoteCommandUsesTheGivenConfig(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "driver", Namespace: "gpu-operator"}}

	_, _, err := executeRemoteCommand(&rest.Config{Host: "https://127.0.0.1:1"}, pod, "true")
	if err == nil {
		t.Fatal("expected an error executing against an unreachable cluster")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error = %q, want it to name the host from the given config", err.Error())
	}
}
