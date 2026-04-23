package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const clusterSyncRequest = "cluster"

type config struct {
	Kubeconfig             string
	Namespaces             []string
	Registries             []string
	TagPrefix              string
	Workers                int
	HealthProbeBindAddress string
	DryRun                 bool
	Once                   bool
	Verbose                bool
}

type assignment struct {
	Namespace   string
	Registry    string
	Repository  string
	Source      string
	Destination string
}

type syncStats struct {
	Total      int64
	Updated    int64
	Skipped    int64
	DryRun     int64
	Conflicted int64
	Failed     int64
}

type app struct {
	cfg            config
	logger         *slog.Logger
	client         ctrlclient.Client
	namespaceAllow map[string]struct{}
	registryAllow  map[string]struct{}
	craneOptions   []crane.Option
	ready          atomic.Bool
}

type clusterReconciler struct {
	app *app
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.Verbose)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scheme, err := newScheme()
	if err != nil {
		logger.Error("scheme init failed", "error", err)
		os.Exit(1)
	}

	restCfg, err := buildRESTConfig(cfg.Kubeconfig)
	if err != nil {
		logger.Error("kubernetes config init failed", "error", err)
		os.Exit(1)
	}

	apiClient, err := ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		logger.Error("kubernetes client init failed", "error", err)
		os.Exit(1)
	}

	application := &app{
		cfg:            cfg,
		logger:         logger,
		client:         apiClient,
		namespaceAllow: toSet(cfg.Namespaces),
		registryAllow:  normalizeRegistries(cfg.Registries),
		craneOptions: []crane.Option{
			crane.WithAuthFromKeychain(authn.DefaultKeychain),
		},
	}

	if err := application.run(ctx, restCfg, scheme); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("run failed", "error", err)
		os.Exit(1)
	}
}

func parseConfig() (config, error) {
	return parseConfigArgs(os.Args[1:])
}

func parseConfigArgs(args []string) (config, error) {
	var cfg config
	var namespaces string
	var registries string
	var registry string

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "Path to kubeconfig. Empty tries in-cluster config first, then ~/.kube/config.")
	fs.StringVar(&namespaces, "namespaces", "", "Comma-separated namespace allow list. Empty means all namespaces.")
	fs.StringVar(&registries, "registries", "", "Comma-separated registry allow list. Empty means all registries.")
	fs.StringVar(&registry, "registry", "", "Alias for --registries with single registry value.")
	fs.StringVar(&cfg.TagPrefix, "tag-prefix", "active", "Destination tag prefix. Final tag becomes <prefix>-<namespace>.")
	fs.IntVar(&cfg.Workers, "workers", 4, "Concurrent registry tag workers.")
	fs.StringVar(&cfg.HealthProbeBindAddress, "health-probe-bind-address", ":8081", "Health probe bind address. Set 0 to disable.")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "Log planned tag updates without writing to registry.")
	fs.BoolVar(&cfg.Once, "once", false, "Run single sync then exit.")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "Enable debug logs.")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	cfg.Namespaces = splitCSV(namespaces)
	cfg.Registries = append(splitCSV(registries), splitCSV(registry)...)
	cfg.TagPrefix = strings.TrimSpace(cfg.TagPrefix)

	if cfg.TagPrefix == "" {
		return cfg, errors.New("tag-prefix required")
	}
	if cfg.Workers < 1 {
		return cfg, errors.New("workers must be >= 1")
	}

	return cfg, nil
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

func buildRESTConfig(kubeconfig string) (*rest.Config, error) {
	if explicit := strings.TrimSpace(kubeconfig); explicit != "" {
		return clientcmd.BuildConfigFromFlags("", explicit)
	}

	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	fallback := defaultKubeconfig()
	if fallback == "" {
		return nil, err
	}

	return clientcmd.BuildConfigFromFlags("", fallback)
}

func defaultKubeconfig() string {
	if kubeconfig, ok := os.LookupEnv("KUBECONFIG"); ok {
		for _, path := range filepath.SplitList(kubeconfig) {
			path = strings.TrimSpace(path)
			if path != "" {
				return path
			}
		}
	}

	home := homedir.HomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func (a *app) run(ctx context.Context, restCfg *rest.Config, scheme *runtime.Scheme) error {
	a.logger.Info("tracker started",
		"mode", modeName(a.cfg.Once),
		"namespaces", displayOrAll(a.cfg.Namespaces),
		"registries", displayOrAll(a.cfg.Registries),
		"tagPrefix", a.cfg.TagPrefix,
		"workers", a.cfg.Workers,
		"healthProbeBindAddress", a.cfg.HealthProbeBindAddress,
		"dryRun", a.cfg.DryRun,
	)

	if a.cfg.Once {
		return a.syncOnce(ctx)
	}

	return a.watchPods(ctx, restCfg, scheme)
}

func (a *app) syncOnce(ctx context.Context) error {
	return a.syncWithClient(ctx, a.client)
}

func (a *app) watchPods(ctx context.Context, restCfg *rest.Config, scheme *runtime.Scheme) error {
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: a.cfg.HealthProbeBindAddress,
		Cache:                  a.cacheOptions(),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	a.client = mgr.GetClient()
	a.ready.Store(false)

	if a.cfg.HealthProbeBindAddress != "0" {
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("add health check: %w", err)
		}
		if err := mgr.AddReadyzCheck("startup-sync", a.readyzCheck); err != nil {
			return fmt.Errorf("add readiness check: %w", err)
		}
	}

	reconciler := &clusterReconciler{app: a}
	if err := builder.TypedControllerManagedBy[string](mgr).
		Named("pod_tracker").
		Watches(
			&v1.Pod{},
			handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, obj ctrlclient.Object) []string {
				pod, ok := obj.(*v1.Pod)
				if !ok || !a.namespaceAllowed(pod.Namespace) {
					return nil
				}
				return []string{clusterSyncRequest}
			}),
			builder.WithPredicates(a.podEventPredicate()),
		).
		WithOptions(controller.TypedOptions[string]{MaxConcurrentReconciles: 1}).
		Complete(reconciler); err != nil {
		return fmt.Errorf("create controller: %w", err)
	}

	if err := mgr.Add(manager.RunnableFunc(func(runCtx context.Context) error {
		if !mgr.GetCache().WaitForCacheSync(runCtx) {
			if runCtx.Err() != nil {
				return runCtx.Err()
			}
			return errors.New("manager cache sync failed")
		}

		a.logger.Info("controller-runtime cache synced")
		if err := a.syncWithClient(runCtx, mgr.GetClient()); err != nil {
			return err
		}
		a.ready.Store(true)

		<-runCtx.Done()
		return nil
	})); err != nil {
		return fmt.Errorf("add startup sync runnable: %w", err)
	}

	return mgr.Start(ctx)
}

func (a *app) readyzCheck(_ *http.Request) error {
	if a.ready.Load() {
		return nil
	}
	return errors.New("initial sync not complete")
}

func (a *app) cacheOptions() ctrlcache.Options {
	if len(a.cfg.Namespaces) == 0 {
		return ctrlcache.Options{}
	}

	namespaces := make(map[string]ctrlcache.Config, len(a.cfg.Namespaces))
	for _, namespace := range a.cfg.Namespaces {
		namespaces[namespace] = ctrlcache.Config{}
	}

	return ctrlcache.Options{DefaultNamespaces: namespaces}
}

func (r *clusterReconciler) Reconcile(ctx context.Context, request string) (ctrl.Result, error) {
	if request != clusterSyncRequest {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.app.syncWithClient(ctx, r.app.client)
}

func (a *app) syncWithClient(ctx context.Context, reader ctrlclient.Reader) error {
	started := time.Now()
	podList := &v1.PodList{}
	if err := reader.List(ctx, podList); err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	assignments, conflicts := a.buildAssignments(podList.Items)
	stats := syncStats{
		Total:      int64(len(assignments)),
		Conflicted: int64(len(conflicts)),
	}

	for _, conflict := range conflicts {
		a.logger.Warn("destination conflict; skipped", "details", conflict)
	}

	err := a.processAssignments(ctx, assignments, &stats)

	a.logger.Info("sync complete",
		"duration", time.Since(started),
		"assignments", stats.Total,
		"updated", stats.Updated,
		"skipped", stats.Skipped,
		"dry-run", stats.DryRun,
		"conflicted", stats.Conflicted,
		"failed", stats.Failed,
	)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return nil
}

func (a *app) podEventPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*v1.Pod)
			return ok && a.namespaceAllowed(pod.Namespace)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			pod, ok := e.Object.(*v1.Pod)
			return ok && a.namespaceAllowed(pod.Namespace)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, oldOK := e.ObjectOld.(*v1.Pod)
			newPod, newOK := e.ObjectNew.(*v1.Pod)
			if !oldOK || !newOK {
				return true
			}
			if !a.namespaceAllowed(newPod.Namespace) && !a.namespaceAllowed(oldPod.Namespace) {
				return false
			}
			return podTrackingState(oldPod) != podTrackingState(newPod)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			pod, ok := e.Object.(*v1.Pod)
			return ok && a.namespaceAllowed(pod.Namespace)
		},
	}
}

func (a *app) buildAssignments(pods []v1.Pod) ([]assignment, []string) {
	assignmentsByDestination := make(map[string]assignment)
	conflicts := make(map[string]map[string]struct{})

	for _, pod := range pods {
		if !a.namespaceAllowed(pod.Namespace) || !podActiveForTracking(&pod) {
			continue
		}

		for _, image := range podImages(pod) {
			ref, err := name.ParseReference(image, name.WeakValidation)
			if err != nil {
				a.logger.Warn("image parse failed; skipped", "namespace", pod.Namespace, "pod", pod.Name, "image", image, "error", err)
				continue
			}

			registry := ref.Context().Registry.Name()
			if !a.registryAllowed(registry) {
				continue
			}

			destinationTag := fmt.Sprintf("%s-%s", a.cfg.TagPrefix, pod.Namespace)
			destination := ref.Context().Tag(destinationTag).Name()
			candidate := assignment{
				Namespace:   pod.Namespace,
				Registry:    registry,
				Repository:  ref.Context().Name(),
				Source:      ref.Name(),
				Destination: destination,
			}

			if _, alreadyConflicted := conflicts[destination]; alreadyConflicted {
				conflicts[destination][candidate.Source] = struct{}{}
				continue
			}

			existing, found := assignmentsByDestination[destination]
			if !found {
				assignmentsByDestination[destination] = candidate
				continue
			}

			if existing.Source == candidate.Source {
				continue
			}

			if _, ok := conflicts[destination]; !ok {
				conflicts[destination] = map[string]struct{}{
					existing.Source: {},
				}
			}
			conflicts[destination][candidate.Source] = struct{}{}
			delete(assignmentsByDestination, destination)
		}
	}

	assignments := make([]assignment, 0, len(assignmentsByDestination))
	for _, item := range assignmentsByDestination {
		assignments = append(assignments, item)
	}
	sort.Slice(assignments, func(i, j int) bool {
		return assignments[i].Destination < assignments[j].Destination
	})

	conflictList := make([]string, 0, len(conflicts))
	for destination, refs := range conflicts {
		values := make([]string, 0, len(refs))
		for ref := range refs {
			values = append(values, ref)
		}
		sort.Strings(values)
		conflictList = append(conflictList, fmt.Sprintf("%s <= %s", destination, strings.Join(values, ", ")))
	}
	sort.Strings(conflictList)

	return assignments, conflictList
}

func (a *app) processAssignments(ctx context.Context, assignments []assignment, stats *syncStats) error {
	jobs := make(chan assignment)
	errCh := make(chan error, len(assignments))
	var wg sync.WaitGroup

	for i := 0; i < a.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if err := a.syncAssignment(ctx, item, stats); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						continue
					}

					a.logger.Error("tag sync failed",
						"namespace", item.Namespace,
						"repository", item.Repository,
						"source", item.Source,
						"destination", item.Destination,
						"error", err,
					)
					atomic.AddInt64(&stats.Failed, 1)
					errCh <- fmt.Errorf("%s -> %s: %w", item.Source, item.Destination, err)
				}
			}
		}()
	}

	for _, item := range assignments {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(errCh)
			return errors.Join(collectErrors(errCh)...)
		case jobs <- item:
		}
	}

	close(jobs)
	wg.Wait()
	close(errCh)
	return errors.Join(collectErrors(errCh)...)
}

func (a *app) syncAssignment(ctx context.Context, item assignment, stats *syncStats) error {
	craneOpts := append(a.craneOptions, crane.WithContext(ctx))

	if a.cfg.DryRun {
		a.logger.Info("dry-run tag sync",
			"namespace", item.Namespace,
			"source", item.Source,
			"destination", item.Destination,
		)
		atomic.AddInt64(&stats.DryRun, 1)
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	sourceDigest, err := crane.Digest(item.Source, craneOpts...)
	if err != nil {
		return fmt.Errorf("resolve source digest: %w", err)
	}

	destinationDigest, err := crane.Digest(item.Destination, craneOpts...)
	if err == nil && destinationDigest == sourceDigest {
		a.logger.Debug("destination already current", "destination", item.Destination, "digest", sourceDigest)
		atomic.AddInt64(&stats.Skipped, 1)
		return nil
	}

	if err := crane.Copy(item.Source, item.Destination, craneOpts...); err != nil {
		return fmt.Errorf("copy image: %w", err)
	}

	a.logger.Info("tag updated",
		"namespace", item.Namespace,
		"source", item.Source,
		"destination", item.Destination,
		"digest", sourceDigest,
	)
	atomic.AddInt64(&stats.Updated, 1)
	return nil
}

func (a *app) namespaceAllowed(namespace string) bool {
	if len(a.namespaceAllow) == 0 {
		return true
	}
	_, ok := a.namespaceAllow[namespace]
	return ok
}

func (a *app) registryAllowed(registry string) bool {
	if len(a.registryAllow) == 0 {
		return true
	}
	_, ok := a.registryAllow[registry]
	return ok
}

func podImages(pod v1.Pod) []string {
	seen := make(map[string]struct{})
	images := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers)+len(pod.Spec.EphemeralContainers))

	appendImage := func(image string) {
		image = strings.TrimSpace(image)
		if image == "" {
			return
		}
		if _, ok := seen[image]; ok {
			return
		}
		seen[image] = struct{}{}
		images = append(images, image)
	}

	for _, container := range pod.Spec.InitContainers {
		appendImage(container.Image)
	}
	for _, container := range pod.Spec.Containers {
		appendImage(container.Image)
	}
	for _, container := range pod.Spec.EphemeralContainers {
		appendImage(container.Image)
	}

	return images
}

func podTrackingState(pod *v1.Pod) string {
	images := podImages(*pod)
	sort.Strings(images)
	return strings.Join([]string{
		pod.Namespace,
		string(pod.Status.Phase),
		strconv.FormatBool(pod.DeletionTimestamp != nil),
		strings.Join(images, ","),
	}, "|")
}

func podActiveForTracking(pod *v1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}

	switch pod.Status.Phase {
	case v1.PodFailed, v1.PodSucceeded:
		return false
	default:
		return true
	}
}

func collectErrors(errCh <-chan error) []error {
	errs := make([]error, 0)
	for err := range errCh {
		errs = append(errs, err)
	}
	return errs
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func toSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func normalizeRegistries(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}

	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		set[value] = struct{}{}
		registry, err := name.NewRegistry(value, name.WeakValidation)
		if err == nil {
			set[registry.Name()] = struct{}{}
		}
	}
	return set
}

func displayOrAll(values []string) string {
	if len(values) == 0 {
		return "all"
	}
	return strings.Join(values, ",")
}

func modeName(once bool) string {
	if once {
		return "once"
	}
	return "watch"
}
