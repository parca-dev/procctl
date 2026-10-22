// Package e2e runs the procctl end-to-end scenarios against a
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
// Set E2E_KEEP_CLUSTER=1 to keep the kind cluster afterwards, and
// E2E_KIND_IMAGE (e.g. kindest/node:v1.35.0) to pick the node image of a new
// one (default: the kind release's own).
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support"
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
			envfuncs.CreateClusterWithOpts(kind.NewProvider(), kindCluster, kindOpts()...),
			envfuncs.LoadImageToCluster(kindCluster, "procctl-testagent:dev"),
			envfuncs.LoadImageToCluster(kindCluster, "procctl-testclient:dev"),
		)
		if os.Getenv("E2E_KEEP_CLUSTER") == "" {
			testenv.Finish(envfuncs.DestroyCluster(kindCluster))
		}
	}
	testenv.Setup(func(ctx context.Context, c *envconf.Config) (context.Context, error) {
		kubeconfig = c.KubeconfigFile()
		// Every scenario relies on its agent and its clients sharing a node.
		out, err := kubectlErr("get", "nodes", "-o", "name")
		if err != nil {
			return ctx, fmt.Errorf("listing nodes: %v: %s", err, out)
		}
		if n := len(strings.Fields(out)); n != 1 {
			return ctx, fmt.Errorf("the suite needs a one-node cluster; this one has %d nodes", n)
		}
		return ctx, nil
	})
	os.Exit(testenv.Run(m))
}

func kindOpts() []support.ClusterOpts {
	if img := os.Getenv("E2E_KIND_IMAGE"); img != "" {
		return []support.ClusterOpts{kind.WithImage(img)}
	}
	return nil
}
