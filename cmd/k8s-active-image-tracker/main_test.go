package main

import (
	"flag"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty", input: "", want: nil},
		{name: "whitespace", input: "   ", want: nil},
		{name: "multiple", input: "a,b,c", want: []string{"a", "b", "c"}},
		{name: "trimmed", input: " a , b , c ", want: []string{"a", "b", "c"}},
		{name: "skip empty entries", input: "a,,b", want: []string{"a", "b"}},
		{name: "single", input: "single", want: []string{"single"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCSV(tt.input)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("splitCSV(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeRegistries(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := normalizeRegistries(nil); got != nil {
			t.Fatalf("normalizeRegistries(nil) = %v, want nil", got)
		}
	})

	t.Run("docker hub adds canonical host", func(t *testing.T) {
		got := normalizeRegistries([]string{"docker.io"})
		if _, ok := got["docker.io"]; !ok {
			t.Fatalf("normalizeRegistries returned %v, want docker.io", got)
		}
		if _, ok := got["index.docker.io"]; !ok {
			t.Fatalf("normalizeRegistries returned %v, want index.docker.io", got)
		}
	})

	t.Run("keeps unchanged registry", func(t *testing.T) {
		got := normalizeRegistries([]string{"ghcr.io"})
		if _, ok := got["ghcr.io"]; !ok {
			t.Fatalf("normalizeRegistries returned %v, want ghcr.io", got)
		}
		if len(got) != 1 {
			t.Fatalf("normalizeRegistries returned %v, want single entry", got)
		}
	})
}

func TestParseConfigArgs_RegistryAlias(t *testing.T) {
	cfg, err := parseConfigArgs([]string{"--registry", "ghcr.io", "--namespaces", "payments"})
	if err != nil {
		t.Fatalf("parseConfigArgs() error = %v", err)
	}

	if !slices.Equal(cfg.Registries, []string{"ghcr.io"}) {
		t.Fatalf("cfg.Registries = %v, want [ghcr.io]", cfg.Registries)
	}
	if !slices.Equal(cfg.Namespaces, []string{"payments"}) {
		t.Fatalf("cfg.Namespaces = %v, want [payments]", cfg.Namespaces)
	}
}

func TestParseConfigArgs_UsesDedicatedFlagSet(t *testing.T) {
	old := flag.CommandLine
	defer func() { flag.CommandLine = old }()

	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	flag.CommandLine.String("kubeconfig", "", "already registered elsewhere")

	cfg, err := parseConfigArgs([]string{"--kubeconfig", "/tmp/config"})
	if err != nil {
		t.Fatalf("parseConfigArgs() error = %v", err)
	}
	if cfg.Kubeconfig != "/tmp/config" {
		t.Fatalf("cfg.Kubeconfig = %q, want %q", cfg.Kubeconfig, "/tmp/config")
	}
}

func TestParseConfigArgs_TrackJobOwnedPods(t *testing.T) {
	cfg, err := parseConfigArgs([]string{"--track-job-owned-pods"})
	if err != nil {
		t.Fatalf("parseConfigArgs() error = %v", err)
	}
	if !cfg.TrackJobOwnedPods {
		t.Fatal("cfg.TrackJobOwnedPods = false, want true")
	}
}

func TestDefaultKubeconfig_UsesKUBECONFIGEnv(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/first:/tmp/second")

	if got := defaultKubeconfig(); got != "/tmp/first" {
		t.Fatalf("defaultKubeconfig() = %q, want %q", got, "/tmp/first")
	}
}

func TestDefaultKubeconfig_FallsBackToHome(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".kube", "config")
	if got := defaultKubeconfig(); got != want {
		t.Fatalf("defaultKubeconfig() = %q, want %q", got, want)
	}
}

func TestPodImages(t *testing.T) {
	pod := v1.Pod{
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{{Image: "ghcr.io/acme/init:1.0"}},
			Containers: []v1.Container{
				{Image: "ghcr.io/acme/api:1.0"},
				{Image: "ghcr.io/acme/sidecar:2.0"},
				{Image: "ghcr.io/acme/api:1.0"},
				{Image: "  "},
			},
			EphemeralContainers: []v1.EphemeralContainer{{
				EphemeralContainerCommon: v1.EphemeralContainerCommon{Image: "ghcr.io/acme/debug:latest"},
			}},
		},
	}

	want := []string{
		"ghcr.io/acme/init:1.0",
		"ghcr.io/acme/api:1.0",
		"ghcr.io/acme/sidecar:2.0",
		"ghcr.io/acme/debug:latest",
	}

	got := podImages(pod)
	if !slices.Equal(got, want) {
		t.Fatalf("podImages() = %v, want %v", got, want)
	}
}

func TestPodImages_Empty(t *testing.T) {
	got := podImages(v1.Pod{})
	if len(got) != 0 {
		t.Fatalf("podImages() = %v, want empty", got)
	}
}

func TestBuildAssignments_Basic(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pods := []v1.Pod{
		newTestPod("payments", "api", "ghcr.io/acme/api:1.4.2"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 1 {
		t.Fatalf("buildAssignments() assignments len = %d, want 1", len(assignments))
	}

	got := assignments[0]
	if got.Source != "ghcr.io/acme/api:1.4.2" {
		t.Fatalf("assignment source = %q, want %q", got.Source, "ghcr.io/acme/api:1.4.2")
	}
	if got.Destination != "ghcr.io/acme/api:live-payments" {
		t.Fatalf("assignment destination = %q, want %q", got.Destination, "ghcr.io/acme/api:live-payments")
	}
}

func TestBuildAssignments_SameSourceTwoPods_NoConflict(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pods := []v1.Pod{
		newTestPod("payments", "pod-a", "ghcr.io/acme/api:1.4.2"),
		newTestPod("payments", "pod-b", "ghcr.io/acme/api:1.4.2"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 1 {
		t.Fatalf("buildAssignments() assignments len = %d, want 1", len(assignments))
	}
}

func TestBuildAssignments_TwoSourcesConflict(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pods := []v1.Pod{
		newTestPod("payments", "pod-a", "ghcr.io/acme/api:1.0"),
		newTestPod("payments", "pod-b", "ghcr.io/acme/api:2.0"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(assignments) != 0 {
		t.Fatalf("buildAssignments() assignments = %v, want none", assignments)
	}
	if len(conflicts) != 1 {
		t.Fatalf("buildAssignments() conflicts = %v, want single conflict", conflicts)
	}
}

func TestBuildAssignments_ThreeSourcesAllConflict(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pods := []v1.Pod{
		newTestPod("payments", "pod-a", "ghcr.io/acme/api:1.0"),
		newTestPod("payments", "pod-b", "ghcr.io/acme/api:2.0"),
		newTestPod("payments", "pod-c", "ghcr.io/acme/api:3.0"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(assignments) != 0 {
		t.Fatalf("buildAssignments() assignments = %v, want none", assignments)
	}
	if len(conflicts) != 1 {
		t.Fatalf("buildAssignments() conflicts = %v, want single conflict", conflicts)
	}

	for _, part := range []string{"1.0", "2.0", "3.0"} {
		if !strings.Contains(conflicts[0], part) {
			t.Fatalf("conflict %q missing %q", conflicts[0], part)
		}
	}
}

func TestBuildAssignments_NamespaceFilter(t *testing.T) {
	a := newTestApp([]string{"allowed"}, nil, "live")
	pods := []v1.Pod{
		newTestPod("allowed", "pod-a", "ghcr.io/acme/api:1.0"),
		newTestPod("blocked", "pod-b", "ghcr.io/acme/api:2.0"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 1 {
		t.Fatalf("buildAssignments() assignments len = %d, want 1", len(assignments))
	}
	if assignments[0].Namespace != "allowed" {
		t.Fatalf("assignment namespace = %q, want %q", assignments[0].Namespace, "allowed")
	}
}

func TestBuildAssignments_RegistryFilter(t *testing.T) {
	a := newTestApp(nil, []string{"ghcr.io"}, "live")
	pods := []v1.Pod{
		newTestPod("payments", "pod-a", "ghcr.io/acme/api:1.0"),
		newTestPod("payments", "pod-b", "quay.io/acme/api:1.0"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 1 {
		t.Fatalf("buildAssignments() assignments len = %d, want 1", len(assignments))
	}
	if assignments[0].Registry != "ghcr.io" {
		t.Fatalf("assignment registry = %q, want %q", assignments[0].Registry, "ghcr.io")
	}
}

func TestBuildAssignments_MultiNamespaceDifferentDestinations(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pods := []v1.Pod{
		newTestPod("ns-a", "pod", "ghcr.io/acme/api:1.0"),
		newTestPod("ns-b", "pod", "ghcr.io/acme/api:1.0"),
	}

	assignments, conflicts := a.buildAssignments(pods)

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 2 {
		t.Fatalf("buildAssignments() assignments len = %d, want 2", len(assignments))
	}

	destinations := []string{assignments[0].Destination, assignments[1].Destination}
	sort.Strings(destinations)
	want := []string{"ghcr.io/acme/api:live-ns-a", "ghcr.io/acme/api:live-ns-b"}
	if !slices.Equal(destinations, want) {
		t.Fatalf("destinations = %v, want %v", destinations, want)
	}
}

func TestBuildAssignments_Sorted(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pods := []v1.Pod{
		newTestPod("zzz", "pod", "ghcr.io/acme/zzz:1.0"),
		newTestPod("aaa", "pod", "ghcr.io/acme/aaa:1.0"),
		newTestPod("mmm", "pod", "ghcr.io/acme/mmm:1.0"),
	}

	assignments, _ := a.buildAssignments(pods)

	for i := 1; i < len(assignments); i++ {
		if assignments[i].Destination < assignments[i-1].Destination {
			t.Fatalf("assignments not sorted at %d: %q before %q", i, assignments[i].Destination, assignments[i-1].Destination)
		}
	}
}

func TestBuildAssignments_SkipsTerminalPods(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	completed := newTestPod("payments", "job", "ghcr.io/acme/api:1.0")
	completed.Status.Phase = v1.PodSucceeded

	assignments, conflicts := a.buildAssignments([]v1.Pod{completed})

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 0 {
		t.Fatalf("buildAssignments() assignments = %v, want none", assignments)
	}
}

func TestBuildAssignments_SkipsDeletingPods(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	deleting := newTestPod("payments", "api", "ghcr.io/acme/api:1.0")
	now := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &now

	assignments, conflicts := a.buildAssignments([]v1.Pod{deleting})

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 0 {
		t.Fatalf("buildAssignments() assignments = %v, want none", assignments)
	}
}

func TestBuildAssignments_SkipsJobOwnedPodsByDefault(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	pod := newTestPod("payments", "job-pod", "ghcr.io/acme/api:1.0")
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "batch-run"}}

	assignments, conflicts := a.buildAssignments([]v1.Pod{pod})

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 0 {
		t.Fatalf("buildAssignments() assignments = %v, want none", assignments)
	}
}

func TestBuildAssignments_KeepsJobOwnedPodsWhenTrackingEnabled(t *testing.T) {
	a := newTestApp(nil, nil, "live")
	a.cfg.TrackJobOwnedPods = true
	pod := newTestPod("payments", "job-pod", "ghcr.io/acme/api:1.0")
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "batch-run"}}

	assignments, conflicts := a.buildAssignments([]v1.Pod{pod})

	if len(conflicts) != 0 {
		t.Fatalf("buildAssignments() conflicts = %v, want none", conflicts)
	}
	if len(assignments) != 1 {
		t.Fatalf("buildAssignments() assignments len = %d, want 1", len(assignments))
	}
}

func TestPodTrackingState_ChangeDetected(t *testing.T) {
	left := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	right := newTestPod("ns", "pod", "ghcr.io/acme/api:2.0")

	if podTrackingState(&left) == podTrackingState(&right) {
		t.Fatal("podTrackingState() should differ when images differ")
	}
}

func TestPodTrackingState_SameImages(t *testing.T) {
	left := newTestPod("ns", "pod-a", "ghcr.io/acme/api:1.0")
	right := newTestPod("ns", "pod-b", "ghcr.io/acme/api:1.0")

	if podTrackingState(&left) != podTrackingState(&right) {
		t.Fatal("podTrackingState() should match for same namespace and images")
	}
}

func TestPodTrackingState_DifferentNamespace(t *testing.T) {
	left := newTestPod("ns-a", "pod", "ghcr.io/acme/api:1.0")
	right := newTestPod("ns-b", "pod", "ghcr.io/acme/api:1.0")

	if podTrackingState(&left) == podTrackingState(&right) {
		t.Fatal("podTrackingState() should differ when namespace differs")
	}
}

func TestPodTrackingState_PhaseChangeDetected(t *testing.T) {
	left := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	right := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	right.Status.Phase = v1.PodSucceeded

	if podTrackingState(&left) == podTrackingState(&right) {
		t.Fatal("podTrackingState() should differ when phase differs")
	}
}

func TestPodTrackingState_DeletionChangeDetected(t *testing.T) {
	left := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	right := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	now := metav1.NewTime(time.Now())
	right.DeletionTimestamp = &now

	if podTrackingState(&left) == podTrackingState(&right) {
		t.Fatal("podTrackingState() should differ when deletion state differs")
	}
}

func TestPodTrackingState_JobOwnerChangeDetected(t *testing.T) {
	left := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	right := newTestPod("ns", "pod", "ghcr.io/acme/api:1.0")
	right.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "batch-run"}}

	if podTrackingState(&left) == podTrackingState(&right) {
		t.Fatal("podTrackingState() should differ when job ownership differs")
	}
}

func newTestApp(namespaces, registries []string, tagPrefix string) *app {
	return &app{
		cfg: config{
			TagPrefix: tagPrefix,
			Workers:   1,
		},
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		namespaceAllow: toSet(namespaces),
		registryAllow:  normalizeRegistries(registries),
	}
}

func newTestPod(namespace, name string, images ...string) v1.Pod {
	containers := make([]v1.Container, len(images))
	for i, image := range images {
		containers[i] = v1.Container{Image: image}
	}

	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1.PodSpec{Containers: containers},
	}
}
