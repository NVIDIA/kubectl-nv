package mustgather

import (
	"os"
	"testing"

	"k8s.io/client-go/rest"
)

func TestGetKubeconfigPath(t *testing.T) {
	tempFile, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tempFile.Name())

	cases := []struct {
		name    string
		input   string
		setEnv  bool
		envVal  string
		want    string
		wantErr bool
	}{
		{"file exists", tempFile.Name(), false, "", tempFile.Name(), false},
		{"env set", "notfound", true, tempFile.Name(), tempFile.Name(), false},
		{"not found", "notfound", false, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setEnv {
				t.Setenv("KUBECONFIG", c.envVal)
			} else {
				os.Unsetenv("KUBECONFIG")
			}
			got, err := getKubeconfigPath(c.input)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCreateKubernetesClient(t *testing.T) {
	tempFile, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tempFile.Name())
	_, err = createKubernetesClient(tempFile.Name())
	if err == nil {
		t.Error("expected error for empty kubeconfig")
	}
}

func TestSetKubernetesDefaults(t *testing.T) {
	cfg := &rest.Config{}
	err := setKubernetesDefaults(cfg)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if cfg.APIPath != "/api" {
		t.Error("APIPath not set")
	}
	if cfg.GroupVersion == nil || cfg.GroupVersion.Version != "v1" {
		t.Error("GroupVersion not set")
	}
}
