package business

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nitishm/engarde/pkg/parser"
	osapps_v1 "github.com/openshift/api/apps/v1"
	security_v1beta1 "istio.io/client-go/pkg/apis/security/v1beta1"
	apps_v1 "k8s.io/api/apps/v1"
	batch_v1 "k8s.io/api/batch/v1"
	core_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kiali/kiali/business/checkers"
	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/kubernetes"
	"github.com/kiali/kiali/kubernetes/cache"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/observability"
	"github.com/kiali/kiali/prometheus"
)

func NewWorkloadService(k8s kubernetes.ClientInterface, prom prometheus.ClientInterface, cache cache.KialiCache, layer *Layer, config *config.Config) *WorkloadService {
	return &WorkloadService{
		k8s:           k8s,
		prom:          prom,
		cache:         cache,
		businessLayer: layer,
		config:        config,
	}
}

// WorkloadService deals with fetching istio/kubernetes workloads related content and convert to kiali model
type WorkloadService struct {
	prom prometheus.ClientInterface
	k8s  kubernetes.ClientInterface
	// Careful not to call the workload service from here as that would be a infinite loop.
	businessLayer *Layer
	// The global kiali cache. This should be passed into the workload service rather than created inside of it.
	cache cache.KialiCache
	// The global kiali config.
	config *config.Config
}

type WorkloadCriteria struct {
	Namespace             string
	WorkloadName          string
	WorkloadType          string
	IncludeIstioResources bool
	IncludeServices       bool
	IncludeHealth         bool
	RateInterval          string
	QueryTime             time.Time
}

// PodLog reports log entries
type PodLog struct {
	Entries        []LogEntry `json:"entries,omitempty"`
	LinesTruncated bool       `json:"linesTruncated,omitempty"`
}

// AccessLogEntry provides parsed info from a single proxy access log entry
type AccessLogEntry struct {
	Timestamp     string `json:"timestamp,omitempty"`
	TimestampUnix int64  `json:"timestampUnix,omitempty"`
}

// LogEntry holds a single log entry
type LogEntry struct {
	Message       string            `json:"message,omitempty"`
	Severity      string            `json:"severity,omitempty"`
	OriginalTime  time.Time         `json:"-"`
	Timestamp     string            `json:"timestamp,omitempty"`
	TimestampUnix int64             `json:"timestampUnix,omitempty"`
	AccessLog     *parser.AccessLog `json:"accessLog,omitempty"`
}

// LogOptions holds query parameter values
type LogOptions struct {
	Duration *time.Duration
	IsProxy  bool // fetching logs for Istio Proxy (Envoy access log)
	MaxLines *int
	core_v1.PodLogOptions
}

var (
	excludedWorkloads map[string]bool

	// Matches an ISO8601 full date
	severityRegexp = regexp.MustCompile(`(?i)ERROR|WARN|DEBUG|TRACE`)
)

func isWorkloadIncluded(workload string) bool {
	if excludedWorkloads == nil {
		return true
	}
	return !excludedWorkloads[workload]
}

// isWorkloadValid returns true if it is a known workload type and it is not configured as excluded
func isWorkloadValid(workloadType string) bool {
	_, knownWorkloadType := controllerOrder[workloadType]
	return knownWorkloadType && isWorkloadIncluded(workloadType)
}

func (in *WorkloadService) getWorkloadValidations(authpolicies []*security_v1beta1.AuthorizationPolicy, workloadsPerNamespace map[string]models.WorkloadList) models.IstioValidations {
	validations := checkers.WorkloadChecker{
		AuthorizationPolicies: authpolicies,
		WorkloadsPerNamespace: workloadsPerNamespace,
	}.Check()

	return validations
}

// GetWorkloadList is the API handler to fetch the list of workloads in a given namespace.
func (in *WorkloadService) GetWorkloadList(ctx context.Context, criteria WorkloadCriteria) (models.WorkloadList, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetWorkloadList",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", criteria.Namespace),
		observability.Attribute("rateInterval", criteria.RateInterval),
		observability.Attribute("queryTime", criteria.QueryTime),
	)
	defer end()

	workloadList := &models.WorkloadList{
		Namespace: models.Namespace{Name: criteria.Namespace, CreationTimestamp: time.Time{}},
		Workloads: []models.WorkloadListItem{},
	}
	var ws models.Workloads
	var authpolicies []*security_v1beta1.AuthorizationPolicy
	var err error

	nFetches := 1
	if criteria.IncludeIstioResources {
		nFetches = 2
	}

	wg := sync.WaitGroup{}
	wg.Add(nFetches)
	errChan := make(chan error, nFetches)

	go func(ctx context.Context) {
		defer wg.Done()
		var err2 error
		ws, err2 = fetchWorkloads(ctx, in.businessLayer, criteria.Namespace, "")
		if err2 != nil {
			log.Errorf("Error fetching Workloads per namespace %s: %s", criteria.Namespace, err2)
			errChan <- err2
		}
	}(ctx)

	istioConfigCriteria := IstioConfigCriteria{
		Namespace:                     criteria.Namespace,
		IncludeAuthorizationPolicies:  true,
		IncludeEnvoyFilters:           true,
		IncludeGateways:               true,
		IncludePeerAuthentications:    true,
		IncludeRequestAuthentications: true,
		IncludeSidecars:               true,
	}
	var istioConfigList models.IstioConfigList

	if criteria.IncludeIstioResources {
		go func(ctx context.Context) {
			defer wg.Done()
			var err2 error
			istioConfigList, err2 = in.businessLayer.IstioConfig.GetIstioConfigList(ctx, istioConfigCriteria)
			if err2 != nil {
				log.Errorf("Error fetching Istio Config per namespace %s: %s", criteria.Namespace, err2)
				errChan <- err2
			}
		}(ctx)
	}

	wg.Wait()
	if len(errChan) != 0 {
		err = <-errChan
		return *workloadList, err
	}

	for _, w := range ws {
		wItem := &models.WorkloadListItem{Health: *models.EmptyWorkloadHealth()}
		wItem.ParseWorkload(w)
		if criteria.IncludeIstioResources {
			wSelector := labels.Set(wItem.Labels).AsSelector().String()
			wItem.IstioReferences = FilterUniqueIstioReferences(FilterWorkloadReferences(wSelector, istioConfigList))
		}
		if criteria.IncludeHealth {
			wItem.Health, err = in.businessLayer.Health.GetWorkloadHealth(ctx, criteria.Namespace, wItem.Name, criteria.RateInterval, criteria.QueryTime, w)
			if err != nil {
				log.Errorf("Error fetching Health in namespace %s for workload %s: %s", criteria.Namespace, wItem.Name, err)
			}
		}
		workloadList.Workloads = append(workloadList.Workloads, *wItem)
	}
	authpolicies = istioConfigList.AuthorizationPolicies
	allWorkloads := map[string]models.WorkloadList{}
	allWorkloads[criteria.Namespace] = *workloadList
	validations := in.getWorkloadValidations(authpolicies, allWorkloads)
	validations.StripIgnoredChecks()
	workloadList.Validations = validations
	return *workloadList, nil
}

func FilterWorkloadReferences(wSelector string, istioConfigList models.IstioConfigList) []*models.IstioValidationKey {
	wkdReferences := make([]*models.IstioValidationKey, 0)
	gwFiltered := kubernetes.FilterGatewaysBySelector(wSelector, istioConfigList.Gateways)
	for _, g := range gwFiltered {
		ref := models.BuildKey(g.Kind, g.Name, g.Namespace)
		exist := false
		for _, r := range wkdReferences {
			exist = exist || *r == ref
		}
		if !exist {
			wkdReferences = append(wkdReferences, &ref)
		}
	}
	apFiltered := kubernetes.FilterAuthorizationPoliciesBySelector(wSelector, istioConfigList.AuthorizationPolicies)
	for _, a := range apFiltered {
		ref := models.BuildKey(a.Kind, a.Name, a.Namespace)
		exist := false
		for _, r := range wkdReferences {
			exist = exist || *r == ref
		}
		if !exist {
			wkdReferences = append(wkdReferences, &ref)
		}
	}
	paFiltered := kubernetes.FilterPeerAuthenticationsBySelector(wSelector, istioConfigList.PeerAuthentications)
	for _, p := range paFiltered {
		ref := models.BuildKey(p.Kind, p.Name, p.Namespace)
		exist := false
		for _, r := range wkdReferences {
			exist = exist || *r == ref
		}
		if !exist {
			wkdReferences = append(wkdReferences, &ref)
		}
	}
	scFiltered := kubernetes.FilterSidecarsBySelector(wSelector, istioConfigList.Sidecars)
	for _, s := range scFiltered {
		ref := models.BuildKey(s.Kind, s.Name, s.Namespace)
		exist := false
		for _, r := range wkdReferences {
			exist = exist || *r == ref
		}
		if !exist {
			wkdReferences = append(wkdReferences, &ref)
		}
	}
	raFiltered := kubernetes.FilterRequestAuthenticationsBySelector(wSelector, istioConfigList.RequestAuthentications)
	for _, ra := range raFiltered {
		ref := models.BuildKey(ra.Kind, ra.Name, ra.Namespace)
		exist := false
		for _, r := range wkdReferences {
			exist = exist || *r == ref
		}
		if !exist {
			wkdReferences = append(wkdReferences, &ref)
		}
	}
	efFiltered := kubernetes.FilterEnvoyFiltersBySelector(wSelector, istioConfigList.EnvoyFilters)
	for _, ef := range efFiltered {
		ref := models.BuildKey(ef.Kind, ef.Name, ef.Namespace)
		exist := false
		for _, r := range wkdReferences {
			exist = exist || *r == ref
		}
		if !exist {
			wkdReferences = append(wkdReferences, &ref)
		}
	}
	return wkdReferences
}

func FilterUniqueIstioReferences(refs []*models.IstioValidationKey) []*models.IstioValidationKey {
	refMap := make(map[models.IstioValidationKey]struct{})
	for _, ref := range refs {
		if _, exist := refMap[*ref]; !exist {
			refMap[*ref] = struct{}{}
		}
	}
	filtered := make([]*models.IstioValidationKey, 0)
	for k := range refMap {
		filtered = append(filtered, &models.IstioValidationKey{
			ObjectType: k.ObjectType,
			Name:       k.Name,
			Namespace:  k.Namespace,
		})
	}
	return filtered
}

// GetWorkload is the API handler to fetch details of a specific workload.
// If includeServices is set true, the Workload will fetch all services related
func (in *WorkloadService) GetWorkload(ctx context.Context, criteria WorkloadCriteria) (*models.Workload, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetWorkload",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", criteria.Namespace),
		observability.Attribute("workloadName", criteria.WorkloadName),
		observability.Attribute("workloadType", criteria.WorkloadType),
		observability.Attribute("includeServices", criteria.IncludeServices),
		observability.Attribute("rateInterval", criteria.RateInterval),
		observability.Attribute("queryTime", criteria.QueryTime),
	)
	defer end()

	ns, err := in.businessLayer.Namespace.GetNamespace(ctx, criteria.Namespace)
	if err != nil {
		return nil, err
	}

	workload, err2 := fetchWorkload(ctx, in.businessLayer, criteria)
	if err2 != nil {
		return nil, err2
	}

	var runtimes []models.Runtime
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		conf := in.config
		app := workload.Labels[conf.IstioLabels.AppLabelName]
		version := workload.Labels[conf.IstioLabels.VersionLabelName]
		runtimes = NewDashboardsService(ns, workload).GetCustomDashboardRefs(criteria.Namespace, app, version, workload.Pods)
	}()

	if criteria.IncludeServices {
		var services *models.ServiceList
		var err error

		criteria := ServiceCriteria{
			Namespace:              criteria.Namespace,
			ServiceSelector:        labels.Set(workload.Labels).String(),
			IncludeOnlyDefinitions: true,
			Health:                 false,
		}
		services, err = in.businessLayer.Svc.GetServiceList(ctx, criteria)
		if err != nil {
			return nil, err
		}
		workload.SetServices(services)
	}

	wg.Wait()
	workload.Runtimes = runtimes

	return workload, nil
}

func (in *WorkloadService) UpdateWorkload(ctx context.Context, namespace string, workloadName string, workloadType string, includeServices bool, jsonPatch string) (*models.Workload, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "UpdateWorkload",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", namespace),
		observability.Attribute("workloadName", workloadName),
		observability.Attribute("workloadType", workloadType),
		observability.Attribute("includeServices", includeServices),
		observability.Attribute("jsonPatch", jsonPatch),
	)
	defer end()

	// Identify controller and apply patch to workload
	err := in.updateWorkload(namespace, workloadName, workloadType, jsonPatch)
	if err != nil {
		return nil, err
	}

	// Cache is stopped after a Create/Update/Delete operation to force a refresh.
	// Refresh once after all the updates have gone through since Update Workload will update
	// every single workload type of that matches name/namespace and we only want to refresh once.
	// TODO: Remove conditional once cache is mandatory
	if in.cache != nil {
		in.cache.Refresh(namespace)
	}

	// After the update we fetch the whole workload
	return in.GetWorkload(ctx, WorkloadCriteria{Namespace: namespace, WorkloadName: workloadName, WorkloadType: workloadType, IncludeServices: includeServices})
}

func (in *WorkloadService) GetPods(ctx context.Context, namespace string, labelSelector string) (models.Pods, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetPods",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", namespace),
		observability.Attribute("labelSelector", labelSelector),
	)
	defer end()

	// Check if user has access to the namespace
	if _, err := in.businessLayer.Namespace.GetNamespace(ctx, namespace); err != nil {
		return nil, err
	}

	var ps []core_v1.Pod
	var err error
	if ps, err = in.k8s.GetPods(namespace, labelSelector); err != nil {
		return nil, err
	}

	pods := models.Pods{}
	pods.Parse(ps)
	return pods, nil
}

func (in *WorkloadService) GetPod(namespace, name string) (*models.Pod, error) {
	p, err := in.k8s.GetPod(namespace, name)
	if err != nil {
		return nil, err
	}
	pod := models.Pod{}
	pod.Parse(p)
	return &pod, nil
}

func (in *WorkloadService) BuildLogOptionsCriteria(container, duration, isProxy, sinceTime, maxLines string) (*LogOptions, error) {
	opts := &LogOptions{}
	opts.PodLogOptions = core_v1.PodLogOptions{Timestamps: true}

	if container != "" {
		opts.Container = container
	}

	if duration != "" {
		duration, err := time.ParseDuration(duration)
		if err != nil {
			return nil, fmt.Errorf("invalid duration [%s]: %v", duration, err)
		}

		opts.Duration = &duration
	}

	opts.IsProxy = isProxy == "true"

	if sinceTime != "" {
		numTime, err := strconv.ParseInt(sinceTime, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid sinceTime [%s]: %v", sinceTime, err)
		}

		opts.SinceTime = &meta_v1.Time{Time: time.Unix(numTime, 0)}
	}

	if maxLines != "" {
		if numLines, err := strconv.Atoi(maxLines); err == nil {
			if numLines > 0 {
				opts.MaxLines = &numLines
			}
		} else {
			return nil, fmt.Errorf("invalid maxLines [%s]: %v", maxLines, err)
		}
	}

	return opts, nil
}

func parseLogLine(line string, isProxy bool, engardeParser *parser.Parser) *LogEntry {
	entry := LogEntry{
		Message:       "",
		Timestamp:     "",
		TimestampUnix: 0,
		Severity:      "INFO",
	}

	splitted := strings.SplitN(line, " ", 2)
	if len(splitted) != 2 {
		log.Debugf("Skipping unexpected log line [%s]", line)
		return nil
	}

	// k8s promises RFC3339 or RFC3339Nano timestamp, ensure RFC3339
	// Split by blanks, to get the miliseconds for sorting, try RFC3339Nano
	entry.Timestamp = splitted[0]

	entry.Message = strings.TrimSpace(splitted[1])
	if entry.Message == "" {
		log.Debugf("Skipping empty log line [%s]", line)
		return nil
	}

	// If we are past the requested time window then stop processing
	parsedTimestamp, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	entry.OriginalTime = parsedTimestamp
	if err != nil {
		log.Debugf("Failed to parse log timestamp (skipping) [%s], %s", entry.Timestamp, err.Error())
		return nil
	}

	severity := severityRegexp.FindString(line)
	if severity != "" {
		entry.Severity = strings.ToUpper(severity)
	}

	// If this is an istio access log, then parse it out. Prefer the access log time over the k8s time
	// as it is the actual time as opposed to the k8s store time.
	if isProxy {
		al, err := engardeParser.Parse(entry.Message)
		// engardeParser.Parse will not throw errors even if no fields
		// were parsed out. Checking here that some fields were actually
		// set before setting the AccessLog to an empty object. See issue #4346.
		if err != nil || isAccessLogEmpty(al) {
			if err != nil {
				log.Debugf("AccessLog parse failure: %s", err.Error())
			}
			// try to parse out the time manually
			tokens := strings.SplitN(entry.Message, " ", 2)
			timestampToken := strings.Trim(tokens[0], "[]")
			t, err := time.Parse(time.RFC3339, timestampToken)
			if err == nil {
				parsedTimestamp = t
			}
		} else {
			entry.AccessLog = al
			t, err := time.Parse(time.RFC3339, al.Timestamp)
			if err == nil {
				parsedTimestamp = t
			}

			// clear accessLog fields we don't need in the returned JSON
			entry.AccessLog.MixerStatus = ""
			entry.AccessLog.OriginalMessage = ""
			entry.AccessLog.ParseError = ""
		}
	}

	// override the timestamp with a simpler format
	precision := strings.Split(parsedTimestamp.String(), ".")
	var milliseconds string
	if len(precision) > 1 {
		ms := precision[1]
		milliseconds = ms[:3]
		splittedms := strings.Fields(milliseconds) // This is needed to avoid invalid dates in ms like 200
		milliseconds = splittedms[0]
	} else {
		milliseconds = "000"
	}

	timestamp := fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d.%s",
		parsedTimestamp.Year(), parsedTimestamp.Month(), parsedTimestamp.Day(),
		parsedTimestamp.Hour(), parsedTimestamp.Minute(), parsedTimestamp.Second(), milliseconds)
	entry.Timestamp = timestamp
	entry.TimestampUnix = parsedTimestamp.UnixMilli()

	return &entry
}

func isAccessLogEmpty(al *parser.AccessLog) bool {
	if al == nil {
		return true
	}

	return (al.Timestamp == "" &&
		al.Authority == "" &&
		al.BytesReceived == "" &&
		al.BytesSent == "" &&
		al.DownstreamLocal == "" &&
		al.DownstreamRemote == "" &&
		al.Duration == "" &&
		al.ForwardedFor == "" &&
		al.Method == "" &&
		al.MixerStatus == "" &&
		al.Protocol == "" &&
		al.RequestId == "" &&
		al.RequestedServer == "" &&
		al.ResponseFlags == "" &&
		al.RouteName == "" &&
		al.StatusCode == "" &&
		al.TcpServiceTime == "" &&
		al.UpstreamCluster == "" &&
		al.UpstreamFailureReason == "" &&
		al.UpstreamLocal == "" &&
		al.UpstreamService == "" &&
		al.UpstreamServiceTime == "" &&
		al.UriParam == "" &&
		al.UriPath == "" &&
		al.UserAgent == "")
}

func fetchWorkloads(ctx context.Context, layer *Layer, namespace string, labelSelector string) (models.Workloads, error) {
	var pods []core_v1.Pod
	var repcon []core_v1.ReplicationController
	var dep []apps_v1.Deployment
	var repset []apps_v1.ReplicaSet
	var depcon []osapps_v1.DeploymentConfig
	var fulset []apps_v1.StatefulSet
	var jbs []batch_v1.Job
	var conjbs []batch_v1.CronJob
	var daeset []apps_v1.DaemonSet

	ws := models.Workloads{}

	// Check if user has access to the namespace (RBAC) in cache scenarios and/or
	// if namespace is accessible from Kiali (Deployment.AccessibleNamespaces)
	if _, err := layer.Namespace.GetNamespace(ctx, namespace); err != nil {
		return nil, err
	}

	wg := sync.WaitGroup{}
	wg.Add(9)
	errChan := make(chan error, 9)

	// Pods are always fetched
	go func() {
		defer wg.Done()
		var err error
		pods, err = layer.k8s.GetPods(namespace, labelSelector)
		if err != nil {
			log.Errorf("Error fetching Pods per namespace %s: %s", namespace, err)
			errChan <- err
		}
	}()

	// Deployments are always fetched
	go func() {
		defer wg.Done()
		var err error
		dep, err = layer.k8s.GetDeployments(namespace)
		if err != nil {
			log.Errorf("Error fetching Deployments per namespace %s: %s", namespace, err)
			errChan <- err
		}
	}()

	// ReplicaSets are always fetched
	go func() {
		defer wg.Done()
		var err error
		repset, err = layer.k8s.GetReplicaSets(namespace)
		if err != nil {
			log.Errorf("Error fetching ReplicaSets per namespace %s: %s", namespace, err)
			errChan <- err
		}
	}()

	// ReplicaControllers are fetched only when included
	go func() {
		defer wg.Done()

		var err error
		if isWorkloadIncluded(kubernetes.ReplicationControllerType) {
			repcon, err = layer.k8s.GetReplicationControllers(namespace)
			if err != nil {
				log.Errorf("Error fetching GetReplicationControllers per namespace %s: %s", namespace, err)
				errChan <- err
			}
		}
	}()

	// DeploymentConfigs are fetched only when included
	go func() {
		defer wg.Done()

		var err error
		if layer.k8s.IsOpenShift() && isWorkloadIncluded(kubernetes.DeploymentConfigType) {
			depcon, err = layer.k8s.GetDeploymentConfigs(namespace)
			if err != nil {
				log.Errorf("Error fetching DeploymentConfigs per namespace %s: %s", namespace, err)
				errChan <- err
			}
		}
	}()

	// StatefulSets are fetched only when included
	go func() {
		defer wg.Done()

		var err error
		if isWorkloadIncluded(kubernetes.StatefulSetType) {
			fulset, err = layer.k8s.GetStatefulSets(namespace)
			if err != nil {
				log.Errorf("Error fetching StatefulSets per namespace %s: %s", namespace, err)
				errChan <- err
			}
		}
	}()

	// CononJobs are fetched only when included
	go func() {
		defer wg.Done()

		var err error
		if isWorkloadIncluded(kubernetes.CronJobType) {
			conjbs, err = layer.k8s.GetCronJobs(namespace)
			if err != nil {
				log.Errorf("Error fetching CronJobs per namespace %s: %s", namespace, err)
				errChan <- err
			}
		}
	}()

	// Jobs are fetched only when included
	go func() {
		defer wg.Done()

		var err error
		if isWorkloadIncluded(kubernetes.JobType) {
			jbs, err = layer.k8s.GetJobs(namespace)
			if err != nil {
				log.Errorf("Error fetching Jobs per namespace %s: %s", namespace, err)
				errChan <- err
			}
		}
	}()

	// DaemonSets are fetched only when included
	go func() {
		defer wg.Done()

		var err error
		if isWorkloadIncluded(kubernetes.DaemonSetType) {
			daeset, err = layer.k8s.GetDaemonSets(namespace)
			if err != nil {
				log.Errorf("Error fetching DaemonSets per namespace %s: %s", namespace, err)
			}
		}
	}()

	wg.Wait()
	if len(errChan) != 0 {
		err := <-errChan
		return ws, err
	}

	// Key: name of controller; Value: type of controller
	controllers := map[string]string{}

	// Find controllers from pods
	for _, pod := range pods {
		if len(pod.OwnerReferences) != 0 {
			for _, ref := range pod.OwnerReferences {
				if ref.Controller != nil && *ref.Controller && isWorkloadIncluded(ref.Kind) {
					if _, exist := controllers[ref.Name]; !exist {
						controllers[ref.Name] = ref.Kind
					} else {
						if controllers[ref.Name] != ref.Kind {
							controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
						}
					}
				}
			}
		} else {
			if _, exist := controllers[pod.Name]; !exist {
				// Pod without controller
				controllers[pod.Name] = "Pod"
			}
		}
	}

	// Resolve ReplicaSets from Deployments
	// Resolve ReplicationControllers from DeploymentConfigs
	// Resolve Jobs from CronJobs
	for controllerName, controllerType := range controllers {
		if controllerType == kubernetes.ReplicaSetType {
			found := false
			iFound := -1
			for i, rs := range repset {
				if rs.Name == controllerName {
					iFound = i
					found = true
					break
				}
			}
			if found && len(repset[iFound].OwnerReferences) > 0 {
				for _, ref := range repset[iFound].OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						if _, exist := controllers[ref.Name]; !exist {
							// For valid owner controllers, delete the child ReplicaSet and add the parent controller,
							// otherwise (for custom controllers), defer to the replica set.
							if isWorkloadValid(ref.Kind) {
								controllers[ref.Name] = ref.Kind
								delete(controllers, controllerName)
							} else {
								log.Debugf("Add ReplicaSet to workload list for custom controller [%s][%s]", ref.Name, ref.Kind)
							}
						} else {
							if controllers[ref.Name] != ref.Kind {
								controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
							}
							delete(controllers, controllerName)
						}
					}
				}
			}
		}
		if controllerType == kubernetes.ReplicationControllerType {
			found := false
			iFound := -1
			for i, rc := range repcon {
				if rc.Name == controllerName {
					iFound = i
					found = true
					break
				}
			}
			if found && len(repcon[iFound].OwnerReferences) > 0 {
				for _, ref := range repcon[iFound].OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						// Delete the child ReplicationController and add the parent controller
						if _, exist := controllers[ref.Name]; !exist {
							controllers[ref.Name] = ref.Kind
						} else {
							if controllers[ref.Name] != ref.Kind {
								controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
							}
						}
						delete(controllers, controllerName)
					}
				}
			}
		}
		if controllerType == kubernetes.JobType {
			found := false
			iFound := -1
			for i, jb := range jbs {
				if jb.Name == controllerName {
					iFound = i
					found = true
					break
				}
			}
			if found && len(jbs[iFound].OwnerReferences) > 0 {
				for _, ref := range jbs[iFound].OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						// Delete the child Job and add the parent controller
						if _, exist := controllers[ref.Name]; !exist {
							controllers[ref.Name] = ref.Kind
						} else {
							if controllers[ref.Name] != ref.Kind {
								controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
							}
						}
						// Jobs are special as deleting CronJob parent doesn't delete children
						// So we need to check that parent exists before to delete children controller
						cnExist := false
						for _, cnj := range conjbs {
							if cnj.Name == ref.Name {
								cnExist = true
								break
							}
						}
						if cnExist {
							delete(controllers, controllerName)
						}
					}
				}
			}
		}
	}

	// Cornercase, check for controllers without pods, to show them as a workload
	var selector labels.Selector
	var selErr error
	if labelSelector != "" {
		selector, selErr = labels.Parse(labelSelector)
		if selErr != nil {
			log.Errorf("%s can not be processed as selector: %v", labelSelector, selErr)
		}
	}
	for _, d := range dep {
		selectorCheck := true
		if selector != nil {
			selectorCheck = selector.Matches(labels.Set(d.Spec.Template.Labels))
		}
		if _, exist := controllers[d.Name]; !exist && selectorCheck {
			controllers[d.Name] = "Deployment"
		}
	}
	for _, rs := range repset {
		selectorCheck := true
		if selector != nil {
			selectorCheck = selector.Matches(labels.Set(rs.Spec.Template.Labels))
		}
		if _, exist := controllers[rs.Name]; !exist && len(rs.OwnerReferences) == 0 && selectorCheck {
			controllers[rs.Name] = "ReplicaSet"
		}
	}
	for _, dc := range depcon {
		selectorCheck := true
		if selector != nil {
			selectorCheck = selector.Matches(labels.Set(dc.Spec.Template.Labels))
		}
		if _, exist := controllers[dc.Name]; !exist && selectorCheck {
			controllers[dc.Name] = "DeploymentConfig"
		}
	}
	for _, rc := range repcon {
		selectorCheck := true
		if selector != nil {
			selectorCheck = selector.Matches(labels.Set(rc.Spec.Template.Labels))
		}
		if _, exist := controllers[rc.Name]; !exist && len(rc.OwnerReferences) == 0 && selectorCheck {
			controllers[rc.Name] = "ReplicationController"
		}
	}
	for _, fs := range fulset {
		selectorCheck := true
		if selector != nil {
			selectorCheck = selector.Matches(labels.Set(fs.Spec.Template.Labels))
		}
		if _, exist := controllers[fs.Name]; !exist && selectorCheck {
			controllers[fs.Name] = "StatefulSet"
		}
	}
	for _, ds := range daeset {
		selectorCheck := true
		if selector != nil {
			selectorCheck = selector.Matches(labels.Set(ds.Spec.Template.Labels))
		}
		if _, exist := controllers[ds.Name]; !exist && selectorCheck {
			controllers[ds.Name] = "DaemonSet"
		}
	}

	// Build workloads from controllers
	var controllerNames []string
	for k := range controllers {
		controllerNames = append(controllerNames, k)
	}
	sort.Strings(controllerNames)
	for _, controllerName := range controllerNames {
		w := &models.Workload{
			Pods:     models.Pods{},
			Services: []models.ServiceOverview{},
		}
		controllerType := controllers[controllerName]
		// Flag to add a controller if it is found
		cnFound := true
		switch controllerType {
		case kubernetes.DeploymentType:
			found := false
			iFound := -1
			for i, dp := range dep {
				if dp.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(dep[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseDeployment(&dep[iFound])
			} else {
				log.Errorf("Workload %s is not found as Deployment", controllerName)
				cnFound = false
			}
		case kubernetes.ReplicaSetType:
			found := false
			iFound := -1
			for i, rs := range repset {
				if rs.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(repset[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseReplicaSet(&repset[iFound])
			} else {
				log.Errorf("Workload %s is not found as ReplicaSet", controllerName)
				cnFound = false
			}
		case kubernetes.ReplicationControllerType:
			found := false
			iFound := -1
			for i, rc := range repcon {
				if rc.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(repcon[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseReplicationController(&repcon[iFound])
			} else {
				log.Errorf("Workload %s is not found as ReplicationController", controllerName)
				cnFound = false
			}
		case kubernetes.DeploymentConfigType:
			found := false
			iFound := -1
			for i, dc := range depcon {
				if dc.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(depcon[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseDeploymentConfig(&depcon[iFound])
			} else {
				log.Errorf("Workload %s is not found as DeploymentConfig", controllerName)
				cnFound = false
			}
		case kubernetes.StatefulSetType:
			found := false
			iFound := -1
			for i, fs := range fulset {
				if fs.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(fulset[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseStatefulSet(&fulset[iFound])
			} else {
				log.Errorf("Workload %s is not found as StatefulSet", controllerName)
				cnFound = false
			}
		case kubernetes.PodType:
			found := false
			iFound := -1
			for i, pod := range pods {
				if pod.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				w.SetPods([]core_v1.Pod{pods[iFound]})
				w.ParsePod(&pods[iFound])
			} else {
				log.Errorf("Workload %s is not found as Pod", controllerName)
				cnFound = false
			}
		case kubernetes.JobType:
			found := false
			iFound := -1
			for i, jb := range jbs {
				if jb.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(jbs[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseJob(&jbs[iFound])
			} else {
				log.Errorf("Workload %s is not found as Job", controllerName)
				cnFound = false
			}
		case kubernetes.CronJobType:
			found := false
			iFound := -1
			for i, cjb := range conjbs {
				if cjb.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(conjbs[iFound].Spec.JobTemplate.Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseCronJob(&conjbs[iFound])
			} else {
				log.Warningf("Workload %s is not found as CronJob (CronJob could be deleted but children are still in the namespace)", controllerName)
				cnFound = false
			}
		case kubernetes.DaemonSetType:
			found := false
			iFound := -1
			for i, ds := range daeset {
				if ds.Name == controllerName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(daeset[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseDaemonSet(&daeset[iFound])
			} else {
				log.Errorf("Workload %s is not found as Deployment", controllerName)
				cnFound = false
			}
		default:
			// This covers the scenario of a custom controller without replicaset, controlling pods directly.
			// Note that a custom controller with replicaset(s) will return the replicaset(s) as the workloads.
			var cPods []core_v1.Pod
			cPods = kubernetes.FilterPodsByController(controllerName, controllerType, pods)
			if len(cPods) > 0 {
				w.ParsePods(controllerName, controllerType, cPods)
				log.Debugf("Workload %s of type %s has no ReplicaSet as a child controller (this may be ok, but is unusual)", controllerName, controllerType)
			}
			w.SetPods(cPods)
		}

		// Add the Proxy Status to the workload
		for _, pod := range w.Pods {
			if pod.HasIstioSidecar() {
				pod.ProxyStatus = layer.ProxyStatus.GetPodProxyStatus(namespace, pod.Name)
			}
		}

		if cnFound {
			ws = append(ws, w)
		}
	}
	return ws, nil
}

func fetchWorkload(ctx context.Context, layer *Layer, criteria WorkloadCriteria) (*models.Workload, error) {
	var pods []core_v1.Pod
	var repcon []core_v1.ReplicationController
	var dep *apps_v1.Deployment
	var repset []apps_v1.ReplicaSet
	var depcon *osapps_v1.DeploymentConfig
	var fulset *apps_v1.StatefulSet
	var jbs []batch_v1.Job
	var conjbs []batch_v1.CronJob
	var ds *apps_v1.DaemonSet

	wl := &models.Workload{
		Pods:              models.Pods{},
		Services:          []models.ServiceOverview{},
		Runtimes:          []models.Runtime{},
		AdditionalDetails: []models.AdditionalItem{},
		Health:            *models.EmptyWorkloadHealth(),
	}

	// Check if user has access to the namespace (RBAC) in cache scenarios and/or
	// if namespace is accessible from Kiali (Deployment.AccessibleNamespaces)
	if _, err := layer.Namespace.GetNamespace(ctx, criteria.Namespace); err != nil {
		return nil, err
	}

	// Flag used for custom controllers
	// i.e. a third party framework creates its own "Deployment" controller with extra features
	// on this case, Kiali will collect basic info from the ReplicaSet controller
	_, knownWorkloadType := controllerOrder[criteria.WorkloadType]

	wg := sync.WaitGroup{}
	wg.Add(9)
	errChan := make(chan error, 9)

	// Pods are always fetched for all workload types
	go func() {
		defer wg.Done()
		var err error
		pods, err = layer.k8s.GetPods(criteria.Namespace, "")
		if err != nil {
			log.Errorf("Error fetching Pods per namespace %s: %s", criteria.Namespace, err)
			errChan <- err
		}
	}()

	// fetch as Deployment when workloadType is Deployment or unspecified
	go func() {
		defer wg.Done()
		var err error

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.DeploymentType {
			return
		}
		dep, err = layer.k8s.GetDeployment(criteria.Namespace, criteria.WorkloadName)
		if err != nil {
			if errors.IsNotFound(err) {
				dep = nil
			} else {
				log.Errorf("Error fetching Deployment per namespace %s and name %s: %s", criteria.Namespace, criteria.WorkloadName, err)
				errChan <- err
			}
		}
	}()

	// fetch as ReplicaSet(s) when workloadType is ReplicaSet, unspecified, *or custom*
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.ReplicaSetType && knownWorkloadType {
			return
		}
		var err error
		repset, err = layer.k8s.GetReplicaSets(criteria.Namespace)
		if err != nil {
			log.Errorf("Error fetching ReplicaSets per namespace %s: %s", criteria.Namespace, err)
			errChan <- err
		}
	}()

	// fetch as ReplicationControllerType when included, and workloadType is ReplicationControllerType or unspecified
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.ReplicationControllerType {
			return
		}

		var err error
		if isWorkloadIncluded(kubernetes.ReplicationControllerType) {
			repcon, err = layer.k8s.GetReplicationControllers(criteria.Namespace)
			if err != nil {
				log.Errorf("Error fetching GetReplicationControllers per namespace %s: %s", criteria.Namespace, err)
				errChan <- err
			}
		}
	}()

	// fetch as DeploymentConfigType when included, and workloadType is DeploymentConfigType or unspecified
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.DeploymentConfigType {
			return
		}

		var err error
		if layer.k8s.IsOpenShift() && isWorkloadIncluded(kubernetes.DeploymentConfigType) {
			depcon, err = layer.k8s.GetDeploymentConfig(criteria.Namespace, criteria.WorkloadName)
			if err != nil {
				depcon = nil
			}
		}
	}()

	// fetch as StatefulSetType when included, and workloadType is StatefulSetType or unspecified
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.StatefulSetType {
			return
		}

		var err error
		if isWorkloadIncluded(kubernetes.StatefulSetType) {
			fulset, err = layer.k8s.GetStatefulSet(criteria.Namespace, criteria.WorkloadName)
			if err != nil {
				fulset = nil
			}
		}
	}()

	// fetch as CronJobType when included, and workloadType is CronJobType or unspecified
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.CronJobType {
			return
		}

		var err error
		if isWorkloadIncluded(kubernetes.CronJobType) {
			conjbs, err = layer.k8s.GetCronJobs(criteria.Namespace)
			if err != nil {
				log.Errorf("Error fetching CronJobs per namespace %s: %s", criteria.Namespace, err)
				errChan <- err
			}
		}
	}()

	// fetch as JobType when included, and workloadType is JobType or unspecified
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.JobType {
			return
		}

		var err error
		if isWorkloadIncluded(kubernetes.JobType) {
			jbs, err = layer.k8s.GetJobs(criteria.Namespace)
			if err != nil {
				log.Errorf("Error fetching Jobs per namespace %s: %s", criteria.Namespace, err)
				errChan <- err
			}
		}
	}()

	// fetch as DaemonSetType when included, and workloadType is DaemonSetType or unspecified
	go func() {
		defer wg.Done()

		if criteria.WorkloadType != "" && criteria.WorkloadType != kubernetes.DaemonSetType {
			return
		}

		var err error
		if isWorkloadIncluded(kubernetes.DaemonSetType) {
			ds, err = layer.k8s.GetDaemonSet(criteria.Namespace, criteria.WorkloadName)
			if err != nil {
				ds = nil
			}
		}
	}()

	wg.Wait()
	if len(errChan) != 0 {
		err := <-errChan
		return wl, err
	}

	// Key: name of controller; Value: type of controller
	controllers := map[string]string{}

	// Find controllers from pods
	for _, pod := range pods {
		if len(pod.OwnerReferences) != 0 {
			for _, ref := range pod.OwnerReferences {
				if ref.Controller != nil && *ref.Controller && isWorkloadIncluded(ref.Kind) {
					if _, exist := controllers[ref.Name]; !exist {
						controllers[ref.Name] = ref.Kind
					} else {
						if controllers[ref.Name] != ref.Kind {
							controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
						}
					}
				}
			}
		} else {
			if _, exist := controllers[pod.Name]; !exist {
				// Pod without controller
				controllers[pod.Name] = "Pod"
			}
		}
	}

	// Resolve ReplicaSets from Deployments
	// Resolve ReplicationControllers from DeploymentConfigs
	// Resolve Jobs from CronJobs
	for controllerName, controllerType := range controllers {
		if controllerType == kubernetes.ReplicaSetType {
			found := false
			iFound := -1
			for i, rs := range repset {
				if rs.Name == controllerName {
					iFound = i
					found = true
					break
				}
			}
			if found && len(repset[iFound].OwnerReferences) > 0 {
				for _, ref := range repset[iFound].OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						// For valid owner controllers, delete the child ReplicaSet and add the parent controller,
						// otherwise (for custom controllers), defer to the replica set.
						if _, exist := controllers[ref.Name]; !exist {
							if isWorkloadValid(ref.Kind) {
								controllers[ref.Name] = ref.Kind
								delete(controllers, controllerName)
							} else {
								log.Debugf("Use ReplicaSet as workload for custom controller [%s][%s]", ref.Name, ref.Kind)
							}
						} else {
							if controllers[ref.Name] != ref.Kind {
								controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
							}
							delete(controllers, controllerName)
						}
					}
				}
			}
		}
		if controllerType == kubernetes.ReplicationControllerType {
			found := false
			iFound := -1
			for i, rc := range repcon {
				if rc.Name == controllerName {
					iFound = i
					found = true
					break
				}
			}
			if found && len(repcon[iFound].OwnerReferences) > 0 {
				for _, ref := range repcon[iFound].OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						// Delete the child ReplicationController and add the parent controller
						if _, exist := controllers[ref.Name]; !exist {
							controllers[ref.Name] = ref.Kind
						} else {
							if controllers[ref.Name] != ref.Kind {
								controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
							}
						}
						delete(controllers, controllerName)
					}
				}
			}
		}
		if controllerType == kubernetes.JobType {
			found := false
			iFound := -1
			for i, jb := range jbs {
				if jb.Name == controllerName {
					iFound = i
					found = true
					break
				}
			}
			if found && len(jbs[iFound].OwnerReferences) > 0 {
				for _, ref := range jbs[iFound].OwnerReferences {
					if ref.Controller != nil && *ref.Controller {
						// Delete the child Job and add the parent controller
						if _, exist := controllers[ref.Name]; !exist {
							controllers[ref.Name] = ref.Kind
						} else {
							if controllers[ref.Name] != ref.Kind {
								controllers[ref.Name] = controllerPriority(controllers[ref.Name], ref.Kind)
							}
						}
						// Jobs are special as deleting CronJob parent doesn't delete children
						// So we need to check that parent exists before to delete children controller
						cnExist := false
						for _, cnj := range conjbs {
							if cnj.Name == ref.Name {
								cnExist = true
								break
							}
						}
						if cnExist {
							delete(controllers, controllerName)
						}
					}
				}
			}
		}
	}

	// Cornercase, check for controllers without pods, to show them as a workload
	if dep != nil {
		if _, exist := controllers[dep.Name]; !exist {
			controllers[dep.Name] = kubernetes.DeploymentType
		}
	}
	for _, rs := range repset {
		if _, exist := controllers[rs.Name]; !exist && len(rs.OwnerReferences) == 0 {
			controllers[rs.Name] = kubernetes.ReplicaSetType
		}
	}
	if depcon != nil {
		if _, exist := controllers[depcon.Name]; !exist {
			controllers[depcon.Name] = kubernetes.DeploymentConfigType
		}
	}
	for _, rc := range repcon {
		if _, exist := controllers[rc.Name]; !exist && len(rc.OwnerReferences) == 0 {
			controllers[rc.Name] = kubernetes.ReplicationControllerType
		}
	}
	if fulset != nil {
		if _, exist := controllers[fulset.Name]; !exist {
			controllers[fulset.Name] = kubernetes.StatefulSetType
		}
	}
	if ds != nil {
		if _, exist := controllers[ds.Name]; !exist {
			controllers[ds.Name] = kubernetes.DaemonSetType
		}
	}

	// Build workload from controllers

	if _, exist := controllers[criteria.WorkloadName]; exist {
		w := models.Workload{
			Pods:              models.Pods{},
			Services:          []models.ServiceOverview{},
			Runtimes:          []models.Runtime{},
			AdditionalDetails: []models.AdditionalItem{},
			Health:            *models.EmptyWorkloadHealth(),
		}

		// We have a controller with criteria.workloadName but if criteria.WorkloadType is specified and does
		// not match then we may not yet have fetched the workload definition.
		// For known types: respect criteria.WorkloadType and return NotFound if necessary.
		// For custom types: fall through to the default handler and try to get the workload definition working
		// up from the pods or replicas sets.
		// see https://github.com/kiali/kiali/issues/3830
		discoveredControllerType := controllers[criteria.WorkloadName]
		controllerType := discoveredControllerType
		if criteria.WorkloadType != "" && discoveredControllerType != criteria.WorkloadType {
			controllerType = criteria.WorkloadType
		}

		// Handle the known types...
		cnFound := true
		switch controllerType {
		case kubernetes.DeploymentType:
			if dep != nil && dep.Name == criteria.WorkloadName {
				selector := labels.Set(dep.Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseDeployment(dep)
			} else {
				log.Errorf("Workload %s is not found as Deployment", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.ReplicaSetType:
			found := false
			iFound := -1
			for i, rs := range repset {
				if rs.Name == criteria.WorkloadName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(repset[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseReplicaSet(&repset[iFound])
			} else {
				log.Errorf("Workload %s is not found as ReplicaSet", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.ReplicationControllerType:
			found := false
			iFound := -1
			for i, rc := range repcon {
				if rc.Name == criteria.WorkloadName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(repcon[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseReplicationController(&repcon[iFound])
			} else {
				log.Errorf("Workload %s is not found as ReplicationController", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.DeploymentConfigType:
			if depcon != nil && depcon.Name == criteria.WorkloadName {
				selector := labels.Set(depcon.Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseDeploymentConfig(depcon)
			} else {
				log.Errorf("Workload %s is not found as DeploymentConfig", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.StatefulSetType:
			if fulset != nil && fulset.Name == criteria.WorkloadName {
				selector := labels.Set(fulset.Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseStatefulSet(fulset)
			} else {
				log.Errorf("Workload %s is not found as StatefulSet", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.PodType:
			found := false
			iFound := -1
			for i, pod := range pods {
				if pod.Name == criteria.WorkloadName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				w.SetPods([]core_v1.Pod{pods[iFound]})
				w.ParsePod(&pods[iFound])
			} else {
				log.Errorf("Workload %s is not found as Pod", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.JobType:
			found := false
			iFound := -1
			for i, jb := range jbs {
				if jb.Name == criteria.WorkloadName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(jbs[iFound].Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseJob(&jbs[iFound])
			} else {
				log.Errorf("Workload %s is not found as Job", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.CronJobType:
			found := false
			iFound := -1
			for i, cjb := range conjbs {
				if cjb.Name == criteria.WorkloadName {
					found = true
					iFound = i
					break
				}
			}
			if found {
				selector := labels.Set(conjbs[iFound].Spec.JobTemplate.Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseCronJob(&conjbs[iFound])
			} else {
				log.Warningf("Workload %s is not found as CronJob (CronJob could be deleted but children are still in the namespace)", criteria.WorkloadName)
				cnFound = false
			}
		case kubernetes.DaemonSetType:
			if ds != nil && ds.Name == criteria.WorkloadName {
				selector := labels.Set(ds.Spec.Template.Labels).AsSelector()
				w.SetPods(kubernetes.FilterPodsBySelector(selector, pods))
				w.ParseDaemonSet(ds)
			} else {
				log.Errorf("Workload %s is not found as DaemonSet", criteria.WorkloadName)
				cnFound = false
			}
		default:
			// Handle a custom type (criteria.WorkloadType is not a known type).
			// 1. Custom controller with replicaset
			// 2. Custom controller without replicaset controlling pods directly

			// 1. use the controller type found in the Pod resolution and ignore the unknown criteria type
			var cPods []core_v1.Pod
			for _, rs := range repset {
				if discoveredControllerType == kubernetes.ReplicaSetType && criteria.WorkloadName == rs.Name {
					w.ParseReplicaSet(&rs)
				} else {
					rsOwnerRef := meta_v1.GetControllerOf(&rs.ObjectMeta)
					if rsOwnerRef != nil && rsOwnerRef.Name == criteria.WorkloadName && rsOwnerRef.Kind == discoveredControllerType {
						w.ParseReplicaSetParent(&rs, criteria.WorkloadName, discoveredControllerType)
					} else {
						continue
					}
				}
				for _, pod := range pods {
					if meta_v1.IsControlledBy(&pod, &rs) {
						cPods = append(cPods, pod)
					}
				}
				break
			}

			// 2. If no pods we're found for a ReplicaSet type, it's possible the controller
			//    is managing the pods itself i.e. the pod's have an owner ref directly to the controller type.
			if len(cPods) == 0 {
				cPods = kubernetes.FilterPodsByController(criteria.WorkloadName, discoveredControllerType, pods)
				if len(cPods) > 0 {
					w.ParsePods(criteria.WorkloadName, discoveredControllerType, cPods)
					log.Debugf("Workload %s of type %s has not a ReplicaSet as a child controller, it may need a revisit", criteria.WorkloadName, discoveredControllerType)
				}
			}

			w.SetPods(cPods)
		}

		// Add the Proxy Status to the workload
		for _, pod := range w.Pods {
			if pod.HasIstioSidecar() {
				pod.ProxyStatus = layer.ProxyStatus.GetPodProxyStatus(criteria.Namespace, pod.Name)
			}
		}

		if cnFound {
			return &w, nil
		}
	}
	return wl, kubernetes.NewNotFound(criteria.WorkloadName, "Kiali", "Workload")
}

func (in *WorkloadService) updateWorkload(namespace string, workloadName string, workloadType string, jsonPatch string) error {
	// Check if user has access to the namespace (RBAC) in cache scenarios and/or
	// if namespace is accessible from Kiali (Deployment.AccessibleNamespaces)
	if _, err := in.businessLayer.Namespace.GetNamespace(context.TODO(), namespace); err != nil {
		return err
	}

	workloadTypes := []string{
		kubernetes.DeploymentType,
		kubernetes.ReplicaSetType,
		kubernetes.ReplicationControllerType,
		kubernetes.DeploymentConfigType,
		kubernetes.StatefulSetType,
		kubernetes.JobType,
		kubernetes.CronJobType,
		kubernetes.PodType,
		kubernetes.DaemonSetType,
	}

	// workloadType is an optional parameter used to optimize the workload type fetch
	// By default workloads use only the "name" but not the pair "name,type".
	if workloadType != "" {
		found := false
		for _, wt := range workloadTypes {
			if workloadType == wt {
				found = true
				break
			}
		}
		if found {
			workloadTypes = []string{workloadType}
		}
	}

	wg := sync.WaitGroup{}
	wg.Add(len(workloadTypes))
	errChan := make(chan error, len(workloadTypes))

	for _, workloadType := range workloadTypes {
		go func(wkType string) {
			defer wg.Done()
			var err error
			if isWorkloadIncluded(wkType) {
				err = in.k8s.UpdateWorkload(namespace, workloadName, wkType, jsonPatch)
			}
			if err != nil {
				if !errors.IsNotFound(err) {
					log.Errorf("Error fetching %s per namespace %s and name %s: %s", wkType, namespace, workloadName, err)
					errChan <- err
				}
			}
		}(workloadType)
	}

	wg.Wait()
	if len(errChan) != 0 {
		err := <-errChan
		return err
	}

	return nil
}

// KIALI-1730
// This method is used to decide the priority of the controller in the cornercase when two controllers have same labels
// on the selector. Note that this is a situation that user should control as it is described in the documentation:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
// But Istio only identifies one controller as workload (it doesn't note which one).
// Kiali can select one on the list of workloads and other in the details and this should be consistent.
var controllerOrder = map[string]int{
	"Deployment":            6,
	"DeploymentConfig":      5,
	"ReplicaSet":            4,
	"ReplicationController": 3,
	"StatefulSet":           2,
	"Job":                   1,
	"DaemonSet":             0,
	"Pod":                   -1,
}

func controllerPriority(type1, type2 string) string {
	w1, e1 := controllerOrder[type1]
	if !e1 {
		log.Errorf("This controller %s is assigned in a Pod and it's not properly managed", type1)
	}
	w2, e2 := controllerOrder[type2]
	if !e2 {
		log.Errorf("This controller %s is assigned in a Pod and it's not properly managed", type2)
	}
	if w1 >= w2 {
		return type1
	} else {
		return type2
	}
}

// GetWorkloadAppName returns the "Application" name (app label) that relates to a workload
func (in *WorkloadService) GetWorkloadAppName(ctx context.Context, namespace, workload string) (string, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetWorkloadAppName",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", namespace),
		observability.Attribute("workload", workload),
	)
	defer end()

	wkd, err := fetchWorkload(ctx, in.businessLayer, WorkloadCriteria{Namespace: namespace, WorkloadName: workload, WorkloadType: ""})
	if err != nil {
		return "", err
	}

	appLabelName := in.config.IstioLabels.AppLabelName
	app := wkd.Labels[appLabelName]
	return app, nil
}

// streamParsedLogs fetches logs from a container in a pod, parses and decorates each log line with some metadata (of possible) and
// sends the processed lines to the client in JSON format. Results are sent as processing is performed, so in case of any error when
// doing processing the JSON document will be truncated.
func (in *WorkloadService) streamParsedLogs(namespace, name string, opts *LogOptions, w http.ResponseWriter) error {
	k8sOpts := opts.PodLogOptions
	// the k8s API does not support "endTime/beforeTime". So for bounded time ranges we need to
	// discard the logs after sinceTime+duration
	isBounded := opts.Duration != nil

	logsReader, err := in.k8s.StreamPodLogs(namespace, name, &k8sOpts)
	if err != nil {
		return err
	}

	defer func() {
		e := logsReader.Close()
		if e != nil {
			log.Errorf("Error when closing the connection streaming logs of a pod: %s", e.Error())
		}
	}()

	bufferedReader := bufio.NewReader(logsReader)

	var startTime *time.Time
	var endTime *time.Time
	if k8sOpts.SinceTime != nil {
		startTime = &k8sOpts.SinceTime.Time
		if isBounded {
			end := startTime.Add(*opts.Duration)
			endTime = &end
		}
	}

	engardeParser := parser.New(parser.IstioProxyAccessLogsPattern)

	// To avoid high memory usage, the JSON will be written
	// to the HTTP Response as it's received from the cluster API.
	// That is, each log line is parsed, decorated with Kiali's metadata,
	// marshalled to JSON and immediately written to the HTTP Response.
	// This means that it is needed to push HTTP headers and start writing
	// the response body right now and any errors at the middle of the log
	// processing can no longer be informed to the client. So, starting
	// these lines, the best we can do if some error happens is to simply
	// log the error and stop/truncate the response, which will have the
	// effect of sending an incomplete JSON document that the browser will fail
	// to parse. Hopefully, the client/UI can catch the parsing error and
	// properly show an error message about the failure retrieving logs.
	w.Header().Set("Content-Type", "application/json")
	_, writeErr := w.Write([]byte("{\"entries\":[")) // This starts the JSON document
	if writeErr != nil {
		return writeErr
	}

	firstEntry := true
	line, readErr := bufferedReader.ReadString('\n')
	linesWritten := 0
	for ; readErr == nil || (readErr == io.EOF && len(line) > 0); line, readErr = bufferedReader.ReadString('\n') {
		// Abort if we already reached the requested max-lines limit
		if opts.MaxLines != nil && linesWritten >= *opts.MaxLines {
			break
		}

		entry := parseLogLine(line, opts.IsProxy, engardeParser)
		if entry == nil {
			continue
		}

		// If we are past the requested time window then stop processing
		if startTime == nil {
			startTime = &entry.OriginalTime
		}

		if isBounded {
			if endTime == nil {
				end := entry.OriginalTime.Add(*opts.Duration)
				endTime = &end
			}

			if entry.OriginalTime.After(*endTime) {
				break
			}
		}

		// Send to client the processed log line

		response, err := json.Marshal(entry)
		if err != nil {
			// Remember that since the HTTP Response body is already being sent,
			// it is not possible to change the response code. So, log the error
			// and terminate early the response.
			log.Errorf("Error when marshalling JSON while streaming pod logs: %s", err.Error())
			return nil
		}

		if firstEntry {
			firstEntry = false
		} else {
			_, writeErr = w.Write([]byte{','})
			if writeErr != nil {
				// Remember that since the HTTP Response body is already being sent,
				// it is not possible to change the response code. So, log the error
				// and terminate early the response.
				log.Errorf("Error when writing log entries separator: %s", writeErr.Error())
				return nil
			}
		}

		_, writeErr = w.Write(response)
		if writeErr != nil {
			log.Errorf("Error when writing a processed log entry while streaming pod logs: %s", writeErr.Error())
			return nil
		}

		linesWritten++
	}

	if readErr == nil && opts.MaxLines != nil && linesWritten >= *opts.MaxLines {
		// End the JSON document, setting the max-lines truncated flag
		_, writeErr = w.Write([]byte("], \"linesTruncated\": true}"))
	} else {
		// End the JSON document
		_, writeErr = w.Write([]byte("]}"))
	}
	if writeErr != nil {
		log.Errorf("Error when writing the outro of the JSON document while streaming pod logs: %s", err.Error())
	}

	return nil
}

// StreamPodLogs streams pod logs to an HTTP Response given the provided options
func (in *WorkloadService) StreamPodLogs(namespace, name string, opts *LogOptions, w http.ResponseWriter) error {
	return in.streamParsedLogs(namespace, name, opts, w)
}
