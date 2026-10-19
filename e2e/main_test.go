// Package e2e runs the procctl end-to-end scenarios (plan §7) against a
// Kubernetes cluster.
//
// By default it creates (or reuses) a kind cluster named procctl-e2e and loads
// the locally built images procctl-testagent:dev and procctl-testclient:dev
// (`make images`). With -kubeconfig it uses that cluster instead, and expects
// the images to be pullable there (see -image-registry / -image-tag).
//
//	go test ./... -v                          # kind
//	go test ./... -v -args -kubeconfig ~/.kube/config -image-registry ghcr.io/me -image-tag sha
//
// Set E2E_KEEP_CLUSTER=1 to keep the kind cluster afterwards.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
)

const kindCluster = "procctl-e2e"

var (
	testenv       env.Environment
	kubeconfig    string
	onKind        bool
	imageRegistry = flag.String("image-registry", "", "registry for the test images (default: local images loaded into kind)")
	imageTag      = flag.String("image-tag", "dev", "tag of the test images")
)

func TestMain(m *testing.M) {
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testenv = env.NewWithConfig(cfg)
	if cfg.KubeconfigFile() == "" {
		onKind = true
		testenv.Setup(
			envfuncs.CreateCluster(kind.NewProvider(), kindCluster),
			envfuncs.LoadImageToCluster(kindCluster, "procctl-testagent:dev"),
			envfuncs.LoadImageToCluster(kindCluster, "procctl-testclient:dev"),
		)
		if os.Getenv("E2E_KEEP_CLUSTER") == "" {
			testenv.Finish(envfuncs.DestroyCluster(kindCluster))
		}
	}
	testenv.Setup(func(ctx context.Context, c *envconf.Config) (context.Context, error) {
		kubeconfig = c.KubeconfigFile()
		return ctx, nil
	})
	os.Exit(testenv.Run(m))
}
