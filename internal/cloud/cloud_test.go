package cloud

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		n, k int
		want []int
	}{
		{0, 2, []int{0, 0}},
		{5, 2, []int{3, 2}},
		{4, 2, []int{2, 2}},
		{1, 3, []int{1, 0, 0}},
		{3, 1, []int{3}},
		{3, 0, []int{}},
	}
	for _, c := range cases {
		if got := Split(c.n, c.k); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%d,%d)=%v want %v", c.n, c.k, got, c.want)
		}
	}
}

func TestFromEnvRejectsUnknownProvider(t *testing.T) {
	t.Setenv("CLOUD_PROVIDERS", "docker,azure")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestECSRequiresCredentialsAndEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if _, err := NewECSFromEnv(); err == nil {
		t.Fatal("expected error without credentials")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("ECS_ENDPOINT_URL", "")
	if _, err := NewECSFromEnv(); err == nil {
		t.Fatal("expected error without ALB endpoint")
	}
	t.Setenv("ECS_ENDPOINT_URL", "http://alb.example")
	e, err := NewECSFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	st := e.Status(t.Context())
	// No network in tests: status must degrade gracefully, never panic.
	if st.Provider != "aws-fargate" || len(st.Endpoints) != 1 {
		t.Fatalf("status %+v", st)
	}
}
