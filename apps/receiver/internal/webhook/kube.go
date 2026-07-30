package webhook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

func newInClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
			cfg, kerr := loadKubeconfig(kubeconfig)
			if kerr != nil {
				return nil, fmt.Errorf("in-cluster: %w; kubeconfig: %w", err, kerr)
			}
			return kubernetes.NewForConfig(cfg)
		}
		return nil, fmt.Errorf("in-cluster: %w", err)
	}
	return kubernetes.NewForConfig(config)
}

// isNotFound reports whether err is a Kubernetes "not found" error of any kind.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err)
}

func yamlUnmarshal(data []byte, out any) error {
	return yaml.Unmarshal(data, out)
}

func loadKubeconfig(path string) (*rest.Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", abs)
	if err != nil {
		return nil, errors.Join(errors.New("build kubeconfig"), err)
	}
	return cfg, nil
}
