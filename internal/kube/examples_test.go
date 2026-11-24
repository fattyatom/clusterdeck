package kube

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestExampleRestoreTemplateIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../examples/postgres-restore-template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := yaml.UnmarshalStrict(raw, cm); err != nil {
		t.Fatal(err)
	}
	cm.Namespace = "shop"
	c, cs := newClient(t, nil, cm)
	ts, err := c.RestoreTemplates(context.Background())
	if err != nil || len(ts) != 1 || ts[0].Error != "" || ts[0].Params[0].Name != "DUMP" {
		t.Fatalf("templates = %+v, err = %v", ts, err)
	}
	if _, err := c.RunRestore(context.Background(), "shop", "restore-db", map[string]string{"DUMP": "shop-20261124T0017Z.dump"}, "shop/restore-db", "me"); err != nil {
		t.Fatal(err)
	}
	jobs, _ := cs.BatchV1().Jobs("shop").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || jobs.Items[0].GenerateName != "shop-restore-db-" {
		t.Fatalf("jobs = %+v", jobs.Items)
	}
}
