//go:build integration

// Command envtestserver starts an envtest API server with the platform CRDs
// and writes a kubeconfig whose context is named kind-steadmesh, so the
// example roots can be planned (never applied) without a real cluster:
//
//	go run -tags integration ./provider/internal/envtestserver -out out/envtest.kubeconfig
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func main() {
	out := flag.String("out", "envtest.kubeconfig", "kubeconfig to write")
	crds := flag.String("crds", "charts/platform/crds", "CRD directory")
	nss := flag.String("namespaces", "steadmesh-system,steadmesh-example", "namespaces to create")
	flag.Parse()
	env := &envtest.Environment{CRDDirectoryPaths: []string{*crds}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		log.Fatal(err)
	}
	defer env.Stop()
	u, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		log.Fatal(err)
	}
	b, err := u.KubeConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, []byte(strings.ReplaceAll(string(b), "envtest", "kind-steadmesh")), 0o600); err != nil {
		log.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	for _, ns := range strings.Split(*nss, ",") {
		obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns}}}
		if _, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("ready: %s", *out)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
