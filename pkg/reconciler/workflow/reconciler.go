package workflow

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path"
	"strings"

	"github.com/puppetlabs/horsehead/v2/graph"
	"github.com/puppetlabs/horsehead/v2/graph/traverse"
	"github.com/puppetlabs/horsehead/v2/storage"
	"github.com/puppetlabs/nebula-sdk/pkg/workflow/spec/evaluate"
	"github.com/puppetlabs/nebula-sdk/pkg/workflow/spec/parse"
	"github.com/puppetlabs/nebula-sdk/pkg/workflow/spec/resolve"
	nebulav1 "github.com/puppetlabs/nebula-tasks/pkg/apis/nebula.puppet.com/v1"
	relayv1beta1 "github.com/puppetlabs/nebula-tasks/pkg/apis/relay.sh/v1beta1"
	"github.com/puppetlabs/nebula-tasks/pkg/dependency"
	"github.com/puppetlabs/nebula-tasks/pkg/errors"
	stconfigmap "github.com/puppetlabs/nebula-tasks/pkg/state/configmap"
	"github.com/puppetlabs/nebula-tasks/pkg/task"
	tekv1alpha1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	tekv1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	"knative.dev/pkg/apis"
	duckv1beta1 "knative.dev/pkg/apis/duck/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var controllerKind = nebulav1.SchemeGroupVersion.WithKind("WorkflowRun")

const (
	nebulaGroupNamePrefix = "nebula.puppet.com/"
	pipelineRunAnnotation = nebulaGroupNamePrefix + "pipelinerun"
	workflowRunAnnotation = nebulaGroupNamePrefix + "workflowrun"
	workflowRunLabel      = nebulaGroupNamePrefix + "workflow-run-id"
	workflowLabel         = nebulaGroupNamePrefix + "workflow-id"
)

type WorkflowRunStatus string

const (
	WorkflowRunStatusPending    WorkflowRunStatus = "pending"
	WorkflowRunStatusInProgress WorkflowRunStatus = "in-progress"
	WorkflowRunStatusSuccess    WorkflowRunStatus = "success"
	WorkflowRunStatusFailure    WorkflowRunStatus = "failure"
	WorkflowRunStatusCancelled  WorkflowRunStatus = "cancelled"
	WorkflowRunStatusSkipped    WorkflowRunStatus = "skipped"
	WorkflowRunStatusTimedOut   WorkflowRunStatus = "timed-out"
)

const (
	// name for the image pull secret used by system containers, if needed
	imagePullSecretName = "relay-system-docker-registry"

	// PipelineRun annotation indicating the log upload location
	logUploadAnnotationPrefix = "log-archive-"
)

const (
	InterpreterDirective = "#!"
	InterpreterDefault   = InterpreterDirective + "/bin/sh"

	NebulaMountPath       = "/nebula"
	NebulaEntrypointFile  = "entrypoint.sh"
	NebulaSpecFile        = "spec.json"
	NebulaConditionalsKey = "conditionals"
)

const (
	ServiceAccountIdentifierCustomer = "customer"
	ServiceAccountIdentifierSystem   = "system"
)

var (
	defaultCustomerNameservers = []string{
		"1.1.1.1",
		"1.0.0.1",
		"8.8.8.8",
	}
)

const (
	WorkflowRunStateCancel = "cancel"
)

// TODO This needs to be exposed by Tekton in some manner
const (
	// ReasonTimedOut indicates that the PipelineRun has taken longer than its configured
	// timeout
	ReasonTimedOut = "PipelineRunTimeout"

	// ReasonConditionCheckFailed indicates that the reason for the failure status is that the
	// condition check associated to the pipeline task evaluated to false
	ReasonConditionCheckFailed = "ConditionCheckFailed"
)

const (
	conditionScript = `#!/bin/bash
JQ="${JQ:-jq}"

CONDITIONS_URL="${CONDITIONS_URL:-conditions}"
VALUE_NAME="${VALUE_NAME:-success}"
POLLING_INTERVAL="${POLLING_INTERVAL:-5s}"
POLLING_ITERATIONS="${POLLING_ITERATIONS:-1080}"

for i in $(seq ${POLLING_ITERATIONS}); do
  CONDITIONS=$(curl "$METADATA_API_URL/${CONDITIONS_URL}")
  VALUE=$(echo $CONDITIONS | $JQ --arg value "$VALUE_NAME" -r '.[$value]')
  if [ -n "${VALUE}" ]; then
    if [ "$VALUE" = "true" ]; then
      exit 0
    fi
    if [ "$VALUE" = "false" ]; then
      exit 1
    fi
  fi
  sleep ${POLLING_INTERVAL}
done

exit 1
`
)

type podAndTaskName struct {
	PodName  string
	TaskName string
}

type StepTask struct {
	dependsOn  []task.Hash
	conditions []task.Hash
}

type StepTasks map[string]StepTask

type Reconciler struct {
	*dependency.DependencyManager
	Client  client.Client
	Scheme  *runtime.Scheme
	metrics *controllerObservations
}

func NewReconciler(dm *dependency.DependencyManager) *Reconciler {
	return &Reconciler{
		DependencyManager: dm,
		Client:            dm.Manager.GetClient(),
		Scheme:            dm.Manager.GetScheme(),
		metrics:           newControllerObservations(dm.Metrics),
	}
}

func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	klog.Infof("reconciling workflow run %s in %s", req.Name, req.Namespace)
	defer klog.Infof("done reconciling workflow run %s in namespace %s", req.Name, req.Namespace)

	ctx := context.Background()

	wr := &nebulav1.WorkflowRun{}
	if err := r.Client.Get(ctx, req.NamespacedName, wr); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if wr.ObjectMeta.DeletionTimestamp.IsZero() {
		err := r.handleWorkflowRun(ctx, wr)
		if err != nil {
			return ctrl.Result{}, err
		}

		plr := &tekv1beta1.PipelineRun{}
		if err = r.Client.Get(ctx, req.NamespacedName, plr); err != nil {
			if !k8serrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}

		if plr != nil && plr.ObjectMeta.Name == wr.ObjectMeta.Name {
			if areWeDoneYet(plr) {
				var logAnnotations map[string]string

				// Upload the logs that are not defined on the PipelineRun record...
				err := r.metrics.trackDurationWithOutcome(metricWorkflowRunLogUploadDuration, func() error {
					logAnnotations, err = r.uploadLogs(ctx, plr)
					return err
				})
				if nil != err {
					klog.Warning(err)
				}

				for name, value := range logAnnotations {
					metav1.SetMetaDataAnnotation(&wr.ObjectMeta, name, value)
				}

				if err := r.Client.Update(ctx, wr); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		status, err := r.updateWorkflowRunStatus(plr, wr)
		if err != nil {
			klog.Warningf("failed to generate workflow run %s status: %+v", wr.GetName(), err)
			return ctrl.Result{}, err
		}

		wr.Status = *status

		err = r.Client.Status().Update(ctx, wr)
		if err != nil {
			klog.Warningf("failed to update workflow run %s status: %+v", wr.GetName(), err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) uploadLogs(ctx context.Context, plr *tekv1beta1.PipelineRun) (map[string]string, error) {
	logAnnotations := make(map[string]string, 0)

	for _, pt := range extractPodAndTaskNamesFromPipelineRun(plr) {
		annotation := nebulaGroupNamePrefix + logUploadAnnotationPrefix + pt.TaskName
		if _, ok := plr.Annotations[annotation]; ok {
			continue
		}
		containerName := "step-" + pt.TaskName
		logName, err := r.uploadLog(ctx, plr.Namespace, pt.PodName, containerName)
		if nil != err {
			klog.Warningf("Failed to upload log for pod=%s/%s container=%s %+v",
				plr.Namespace,
				pt.PodName,
				containerName,
				err)
			continue
		}

		logAnnotations[annotation] = logName
	}

	return logAnnotations, nil
}

func (r *Reconciler) uploadLog(ctx context.Context, namespace string, podName string, containerName string) (string, error) {
	key := fmt.Sprintf("%s/%s/%s", namespace, podName, containerName)

	// XXX: We can't do this with the dynamic client yet.
	opts := &corev1.PodLogOptions{
		Container: containerName,
	}
	rc, err := r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	storageOpts := storage.PutOptions{
		ContentType: "application/octet-stream",
	}

	err = r.StorageClient.Put(ctx, key, func(w io.Writer) error {
		_, err := io.Copy(w, rc)

		return err
	}, storageOpts)
	if err != nil {
		return "", err
	}

	return key, nil
}

func (r *Reconciler) writeWorkflowState(wr *nebulav1.WorkflowRun, taskHash [sha1.Size]byte, state relayv1beta1.UnstructuredObject) errors.Error {
	// TODO: The metadata API isn't using the controller-runtime framework so we
	// may always need the KubeClient here.
	stm := stconfigmap.New(r.KubeClient, wr.GetNamespace())

	data, err := json.Marshal(state)
	if err != nil {
		return errors.NewServerJSONEncodingError().WithCause(err)
	}

	buf := &bytes.Buffer{}
	buf.Write(data)

	md := &task.Metadata{
		Run:  wr.GetName(),
		Hash: taskHash,
	}

	err = stm.Set(context.TODO(), md, buf)
	if err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	return nil
}

func (r *Reconciler) handleWorkflowRun(ctx context.Context, wr *nebulav1.WorkflowRun) error {
	err := r.initializeWorkflowRun(ctx, wr)
	if err != nil {
		return err
	}

	if wr.ObjectMeta.DeletionTimestamp.IsZero() {
		cancelled := isCancelled(wr)
		if annotation, ok := wr.GetAnnotations()[pipelineRunAnnotation]; !ok && !cancelled {
			plr, err := r.createPipelineRun(ctx, wr)
			if err != nil {
				return err
			}

			pipelineID := wr.Spec.Name
			if wr.Labels == nil {
				wr.Labels = make(map[string]string, 0)
			}
			wr.Labels[pipelineRunAnnotation] = pipelineID

			metav1.SetMetaDataAnnotation(&wr.ObjectMeta, pipelineRunAnnotation, plr.Name)

			if err := r.Client.Update(ctx, wr); err != nil {
				return err
			}
		} else if ok && cancelled {
			var plr tekv1beta1.PipelineRun
			if err := r.Client.Get(ctx, client.ObjectKey{
				Namespace: wr.GetNamespace(),
				Name:      annotation,
			}, &plr); k8serrors.IsNotFound(err) {
				return nil
			} else if err != nil {
				return err
			}

			plr.Spec.Status = tekv1beta1.PipelineRunSpecStatusCancelled
			if err := r.Client.Update(ctx, &plr); err != nil {
				return err
			}
		}
	} else {
		if containsString(wr.ObjectMeta.Finalizers, workflowRunAnnotation) {
			wr.ObjectMeta.Finalizers = removeString(wr.ObjectMeta.Finalizers, workflowRunAnnotation)
			if err := r.Client.Update(ctx, wr); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *Reconciler) initializeWorkflowRun(ctx context.Context, wr *nebulav1.WorkflowRun) error {
	for name, value := range wr.State.Steps {
		thisTask := &task.Task{
			Run:  wr.GetName(),
			Name: name,
		}
		taskHash := thisTask.TaskHash()
		err := r.writeWorkflowState(wr, taskHash, value)
		if err != nil {
			return err
		}
	}

	// If we haven't set the state of the run yet, then we need to ensure all
	// the secret access is set up.
	if wr.Status.Status == "" {
		klog.Infof("unreconciled %s %s", wr.Kind, wr.GetName())

		err := r.metrics.trackDurationWithOutcome(metricWorkflowRunStartUpDuration, func() error {
			if err := r.createAccessResources(ctx, wr); err != nil {
				return err
			}

			return r.initializePipeline(wr)
		})

		if err != nil {
			return err
		}
	}

	return nil
}

func (r *Reconciler) createAccessResources(ctx context.Context, wr *nebulav1.WorkflowRun) error {
	ips, err := r.copyImagePullSecret(wr)
	if err != nil {
		return err
	}

	_, err = r.createServiceAccount(wr, ServiceAccountIdentifierCustomer, nil)
	if err != nil {
		return err
	}

	_, err = r.createServiceAccount(wr, ServiceAccountIdentifierSystem, ips)
	if err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) createPipelineRun(ctx context.Context, wr *nebulav1.WorkflowRun) (*tekv1beta1.PipelineRun, error) {
	klog.Infof("creating PipelineRun for WorkflowRun %s", wr.GetName())
	defer klog.Infof("done creating PipelineRun for WorkflowRun %s", wr.GetName())

	namespace := wr.GetNamespace()

	pipelineRun := &tekv1beta1.PipelineRun{}
	r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: wr.GetName()}, pipelineRun)
	if pipelineRun != nil && pipelineRun != (&tekv1beta1.PipelineRun{}) && pipelineRun.Name != "" {
		// XXX: FIXME? This is an odd check -- it seems to ignore the error condition?
		return pipelineRun, nil
	}

	runID := wr.Spec.Name

	serviceAccounts := make([]tekv1beta1.PipelineRunSpecServiceAccountName, 0)
	for _, step := range wr.Spec.Workflow.Steps {
		if step == nil {
			continue
		}

		thisTask := &task.Task{
			Run:  wr.GetName(),
			Name: step.Name,
		}
		taskHashKey := thisTask.TaskHash().HexEncoding()

		psa := tekv1beta1.PipelineRunSpecServiceAccountName{
			TaskName:           taskHashKey,
			ServiceAccountName: strings.Join([]string{wr.Spec.Workflow.Name, ServiceAccountIdentifierCustomer}, "-"),
		}
		serviceAccounts = append(serviceAccounts, psa)
	}

	pipelineRun = &tekv1beta1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runID,
			Namespace: namespace,
			Labels:    getLabels(wr, nil),
		},
		Spec: tekv1beta1.PipelineRunSpec{
			ServiceAccountName:  strings.Join([]string{wr.Spec.Workflow.Name, ServiceAccountIdentifierSystem}, "-"),
			ServiceAccountNames: serviceAccounts,
			PipelineRef: &tekv1beta1.PipelineRef{
				Name: runID,
			},
			PodTemplate: &tekv1beta1.PodTemplate{
				NodeSelector: map[string]string{
					"nebula.puppet.com/scheduling.customer-ready": "true",
				},
				Tolerations: []corev1.Toleration{
					{
						Key:    "nebula.puppet.com/scheduling.customer-workload",
						Value:  "true",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				DNSPolicy: func(p corev1.DNSPolicy) *corev1.DNSPolicy { return &p }(corev1.DNSNone),
				DNSConfig: &corev1.PodDNSConfig{
					Nameservers: defaultCustomerNameservers,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(wr, pipelineRun, r.Scheme); err != nil {
		return nil, err
	}

	if err := r.Client.Create(ctx, pipelineRun); err != nil {
		return nil, err
	}

	return pipelineRun, nil
}

// TODO Refine/split this logic
func (r *Reconciler) updateWorkflowRunStatus(plr *tekv1beta1.PipelineRun, wr *nebulav1.WorkflowRun) (*nebulav1.WorkflowRunStatus, error) {
	workflowRunSteps := make(map[string]nebulav1.WorkflowRunStatusSummary)
	workflowRunConditions := make(map[string]nebulav1.WorkflowRunStatusSummary)

	status := wr.Status.Status
	if plr != nil && plr.ObjectMeta.Name == wr.ObjectMeta.Name {
		status = string(mapStatus(plr.Status.Status))
	}

	// FIXME Not necessarily true (needs to differentiate between actual failures and cancellations)
	if isCancelled(wr) {
		status = string(WorkflowRunStatusCancelled)
	}

	workflowRunStatus := &nebulav1.WorkflowRunStatus{
		Status:     status,
		Steps:      make(map[string]nebulav1.WorkflowRunStatusSummary),
		Conditions: make(map[string]nebulav1.WorkflowRunStatusSummary),
	}

	if plr != nil && plr.ObjectMeta.Name == wr.ObjectMeta.Name {
		if plr.Status.StartTime != nil {
			workflowRunStatus.StartTime = plr.Status.StartTime
		}
		if plr.Status.CompletionTime != nil {
			workflowRunStatus.CompletionTime = plr.Status.CompletionTime
		}

		for name, taskRun := range plr.Status.TaskRuns {
			for _, condition := range taskRun.ConditionChecks {
				if condition.Status == nil {
					continue
				}

				conditionStep := nebulav1.WorkflowRunStatusSummary{
					Name:   name,
					Status: string(mapStatus(condition.Status.Status)),
				}

				if condition.Status.StartTime != nil {
					conditionStep.StartTime = condition.Status.StartTime
				}
				if condition.Status.CompletionTime != nil {
					conditionStep.CompletionTime = condition.Status.CompletionTime
				}

				if _, ok := workflowRunConditions[taskRun.PipelineTaskName]; ok {
					klog.Warningf("task %s of workflow run %s has extra conditions, skipping processing for additional condition", taskRun.PipelineTaskName, wr.GetName())
				} else {
					workflowRunConditions[taskRun.PipelineTaskName] = conditionStep
				}
			}

			if taskRun.Status == nil {
				continue
			}

			step := nebulav1.WorkflowRunStatusSummary{
				Name:   name,
				Status: string(mapStatus(taskRun.Status.Status)),
			}

			if taskRun.Status.StartTime != nil {
				step.StartTime = taskRun.Status.StartTime
			}
			if taskRun.Status.CompletionTime != nil {
				step.CompletionTime = taskRun.Status.CompletionTime
			}

			workflowRunSteps[taskRun.PipelineTaskName] = step
		}
	}

	steps := graph.NewSimpleDirectedGraphWithFeatures(graph.DeterministicIteration)
	for _, step := range wr.Spec.Workflow.Steps {
		if step == nil {
			continue
		}

		steps.AddVertex(step.Name)
		for _, dep := range step.DependsOn {
			steps.AddVertex(dep)
			steps.Connect(dep, step.Name)
		}

		thisTask := &task.Task{
			Run:  wr.GetName(),
			Name: step.Name,
		}
		taskHashKey := thisTask.TaskHash().HexEncoding()

		if runStep, ok := workflowRunSteps[taskHashKey]; ok {
			workflowRunStatus.Steps[step.Name] = runStep
		} else {
			workflowRunStatus.Steps[step.Name] = nebulav1.WorkflowRunStatusSummary{
				Status: string(WorkflowRunStatusPending),
			}
		}

		if runCondition, ok := workflowRunConditions[taskHashKey]; ok {
			workflowRunStatus.Conditions[step.Name] = runCondition
		}
	}

	return r.enrichResults(workflowRunStatus, steps)
}

func (r *Reconciler) enrichResults(sts *nebulav1.WorkflowRunStatus, steps *graph.SimpleDirectedGraph) (*nebulav1.WorkflowRunStatus, errors.Error) {
	traverse.NewTopologicalOrderTraverser(steps).ForEach(func(next graph.Vertex) error {
		if step, ok := sts.Steps[next.(string)]; ok {
			incoming, _ := steps.IncomingEdgesOf(next)
			incoming.ForEach(func(edge graph.Edge) error {
				source, _ := steps.SourceVertexOf(edge)

				sourceStep := sts.Steps[source.(string)]

				if step.Status == string(WorkflowRunStatusPending) {
					switch sts.Status {
					case string(WorkflowRunStatusCancelled), string(WorkflowRunStatusFailure), string(WorkflowRunStatusTimedOut):
						step.Status = string(WorkflowRunStatusSkipped)
					}

					switch sourceStep.Status {
					case string(WorkflowRunStatusSkipped), string(WorkflowRunStatusFailure):
						step.Status = string(WorkflowRunStatusSkipped)
					}
				}

				return nil
			})

			sts.Steps[next.(string)] = step
		}

		return nil
	})

	return sts, nil
}

func (r *Reconciler) initializePipeline(wr *nebulav1.WorkflowRun) errors.Error {
	klog.Infof("initializing Pipeline %s", wr.GetName())
	defer klog.Infof("done initializing Pipeline %s", wr.GetName())

	if len(wr.Spec.Workflow.Steps) == 0 {
		return nil
	}

	pipeline := &tekv1beta1.Pipeline{}
	if err := r.Client.Get(context.TODO(), client.ObjectKey{
		Namespace: wr.GetNamespace(),
		Name:      wr.GetName(),
	}, pipeline); err != nil && !k8serrors.IsNotFound(err) {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	if pipeline.Name == wr.GetName() {
		return nil
	}

	if _, err := r.createNetworkPolicies(wr.GetNamespace()); err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	if _, err := r.createLimitRange(wr.GetNamespace()); err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	tasks, err := r.createTasks(wr)
	if err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	pipelineTasks, err := r.createPipelineTasks(tasks)
	if err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	pipeline, err = r.createPipeline(wr, pipelineTasks)
	if err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	return nil
}

func (r *Reconciler) createNetworkPolicies(namespace string) ([]*networkingv1.NetworkPolicy, errors.Error) {
	var pols []*networkingv1.NetworkPolicy

	pols = append(pols, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "metadata-api-allow",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "nebula",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":      "nebula",
					"app.kubernetes.io/component": "metadata-api",
				},
			},
			PolicyTypes: []networkingv1.PolicyType{"Ingress", "Egress"},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							// Match all pods in this namespace.
							PodSelector: &metav1.LabelSelector{},
						},
						{
							// Allow the workflow controller to check for this
							// service's status.
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"nebula.puppet.com/network-policy.tasks": "true",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app.kubernetes.io/name":      "nebula-system",
									"app.kubernetes.io/component": "tasks",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: func(p corev1.Protocol) *corev1.Protocol { return &p }(corev1.ProtocolTCP),
							Port:     func(i intstr.IntOrString) *intstr.IntOrString { return &i }(intstr.FromInt(7000)),
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							// Only allow outbound to the tasks namespace.
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"nebula.puppet.com/network-policy.tasks": "true",
								},
							},
						},
					},
				},
			},
		},
	})

	// We need to let the metadata API talk to the Kubernetes master in private
	// clusters, which use RFC 1918 space. The master is not addressable using a
	// label selector, sadly.
	//
	// Per https://github.com/kubernetes/kubernetes/issues/49978, the additive
	// nature of network policy peers should let us include 0.0.0.0/0, exclude
	// 10.0.0.0/8, and then include 10.X.Y.Z/32 (since policies are supposed to
	// be additive). However, as of 2019-12-12, this does not appear to work
	// with GKE's Project Calico networking implementation, so we'll instead
	// filter the master out of our deny list.
	initialDeny := []string{
		"0.0.0.0/8",       // "This host on this network"
		"10.0.0.0/8",      // Private-Use
		"100.64.0.0/10",   // Shared Address Space
		"169.254.0.0/16",  // Link Local
		"172.16.0.0/12",   // Private-Use
		"192.0.0.0/24",    // IETF Protocol Assignments
		"192.0.2.0/24",    // Documentation (TEST-NET-1)
		"192.31.196.0/24", // AS112-v4
		"192.52.193.0/24", // AMT
		"192.168.0.0/16",  // Private-Use
		"192.175.48.0/24", // Direct Delegation AS112 Service
		"198.18.0.0/15",   // Benchmarking
		"198.51.100.0/24", // Documentation (TEST-NET-2)
		"203.0.113.0/24",  // Documentation (TEST-NET-3)
		"240.0.0.0/4",     // Reserved (multicast)
	}

	// Get the cluster master endpoints from kubernetes.default.svc.
	var master corev1.Endpoints
	if err := r.Client.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &master); err != nil {
		return nil, errors.NewWorkflowExecutionError().WithCause(err)
	}

	var masterIPs []net.IP
	for _, subset := range master.Subsets {
		for _, addr := range subset.Addresses {
			ip := net.ParseIP(addr.IP)
			if ip != nil {
				masterIPs = append(masterIPs, ip)
			}
		}
	}

	var filteredDeny []string
	for _, cidr := range initialDeny {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			// This shouldn't happen, but it will get caught by the admission
			// controller anyway.
			filteredDeny = append(filteredDeny, cidr)
			continue
		}

		filtered := ipNetExclude(network, masterIPs)
		for _, filteredNetwork := range filtered {
			filteredDeny = append(filteredDeny, filteredNetwork.String())
		}
	}

	pols = append(pols, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "nebula",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Empty pod selector matches all pods.
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{"Ingress", "Egress"},
			// We omit ingress to deny inbound traffic. Nothing should be
			// connecting to task pods.
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							// Allow all external traffic except RFC 1918 space
							// and IANA special-purpose address registry.
							IPBlock: &networkingv1.IPBlock{
								CIDR:   "0.0.0.0/0",
								Except: filteredDeny,
							},
						},
						{
							// Allow access to the metadata API.
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app.kubernetes.io/name":      "nebula",
									"app.kubernetes.io/component": "metadata-api",
								},
							},
						},
						{
							// Allow access to kube-dns.
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"nebula.puppet.com/network-policy.kube-system": "true",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"k8s-app": "kube-dns",
								},
							},
						},
					},
				},
			},
		},
	})

	for _, pol := range pols {
		if err := r.Client.Create(context.TODO(), pol); err != nil && !k8serrors.IsAlreadyExists(err) {
			return nil, errors.NewWorkflowExecutionError().WithCause(err)
		}
	}

	return pols, nil
}

func (r *Reconciler) createLimitRange(namespace string) (*corev1.LimitRange, errors.Error) {
	// Set some default (fairly generous) CPU and memory limits.

	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: namespace,
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("750m"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("3Gi"),
					},
				},
			},
		},
	}

	if err := r.Client.Create(context.TODO(), lr); err != nil && !k8serrors.IsAlreadyExists(err) {
		return nil, errors.NewWorkflowExecutionError().WithCause(err)
	}

	return lr, nil
}

func (r *Reconciler) createTasks(wr *nebulav1.WorkflowRun) (StepTasks, errors.Error) {
	stepTasks := make(StepTasks)

	// TODO: Configure CoreDNS and a real DNS name here.
	metadataAPIURL := fmt.Sprintf("http://%s", service.Spec.ClusterIP)

	for _, step := range wr.Spec.Workflow.Steps {
		if step == nil {
			continue
		}

		thisTask := &task.Task{
			Run:  wr.GetName(),
			Name: step.Name,
		}
		taskHash := thisTask.TaskHash()

		err := r.createTaskConfigMap(taskHash, wr, step)
		if err != nil {
			return nil, errors.NewWorkflowExecutionError().WithCause(err)
		}

		_, err = r.createTaskFromStep(wr, taskHash, metadataAPIURL, step)
		if err != nil {
			return nil, errors.NewWorkflowExecutionError().WithCause(err)
		}

		dependsOn := make([]task.Hash, 0)
		conditions := make([]task.Hash, 0)

		for _, dependency := range step.DependsOn {
			thisDependency := &task.Task{
				Run:  wr.GetName(),
				Name: dependency,
			}
			dependencyHash := thisDependency.TaskHash()
			dependsOn = append(dependsOn, dependencyHash)
		}

		if step.When.Value() != nil {
			err := r.createCondition(wr, taskHash, metadataAPIURL)
			if err != nil {
				return nil, errors.NewWorkflowExecutionError().WithCause(err)
			}

			conditions = append(conditions, taskHash)
		}

		stepTasks[taskHash.HexEncoding()] = StepTask{
			dependsOn:  dependsOn,
			conditions: conditions,
		}
	}

	return stepTasks, nil
}

func (r *Reconciler) createPipelineTasks(stepTasks StepTasks) ([]tekv1beta1.PipelineTask, errors.Error) {

	pipelineTasks := make([]tekv1beta1.PipelineTask, 0)

	for taskId, stepTask := range stepTasks {
		dependencies, conditions, err := r.getTaskDependencies(stepTask)
		if err != nil {
			return nil, errors.NewWorkflowExecutionError().WithCause(err)
		}

		pipelineTask := tekv1beta1.PipelineTask{
			Name: taskId,
			TaskRef: &tekv1beta1.TaskRef{
				Name: taskId,
			},
			RunAfter:   dependencies,
			Conditions: conditions,
		}

		pipelineTasks = append(pipelineTasks, pipelineTask)
	}

	return pipelineTasks, nil
}

func (r *Reconciler) getTaskDependencies(stepTask StepTask) ([]string, []tekv1beta1.PipelineTaskCondition, errors.Error) {
	dependencies := make([]string, 0)
	conditions := make([]tekv1beta1.PipelineTaskCondition, 0)

	for _, dependsOn := range stepTask.dependsOn {
		dependencies = append(dependencies, dependsOn.HexEncoding())
	}

	for _, condition := range stepTask.conditions {
		pipelineTaskCondition := tekv1beta1.PipelineTaskCondition{
			ConditionRef: condition.HexEncoding(),
		}
		conditions = append(conditions, pipelineTaskCondition)
	}

	return dependencies, conditions, nil
}

func (r *Reconciler) createCondition(wr *nebulav1.WorkflowRun, taskHash task.Hash, metadataAPIURL string) errors.Error {
	taskHashKey := taskHash.HexEncoding()

	condition := &tekv1alpha1.Condition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            taskHashKey,
			Namespace:       wr.GetNamespace(),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(wr, controllerKind)},
			Labels: map[string]string{
				"nebula.puppet.com/task.hash": taskHashKey,
				"nebula.puppet.com/run":       wr.GetName(),
			},
		},
		Spec: tekv1alpha1.ConditionSpec{
			Check: tekv1beta1.Step{
				Container: corev1.Container{
					Image: "projectnebula/core",
					Name:  taskHashKey,
					Env:   buildEnvironmentVariables(metadataAPIURL),
				},
				Script: conditionScript,
			},
		},
	}

	if err := r.Client.Create(context.TODO(), condition); err != nil && !k8serrors.IsAlreadyExists(err) {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	return nil
}

func (r *Reconciler) createTaskConfigMap(taskHash task.Hash, wr *nebulav1.WorkflowRun, step *nebulav1.WorkflowStep) errors.Error {
	configMapData, _ := getConfigMapData(wr.Spec.Workflow.Parameters, wr.Spec.Parameters, step)
	_, err := r.createConfigMap(wr, taskHash, configMapData)
	if err != nil {
		return errors.NewWorkflowExecutionError().WithCause(err)
	}

	return nil
}

func (r *Reconciler) createTaskFromStep(wr *nebulav1.WorkflowRun, taskHash task.Hash, metadataAPIURL string, step *nebulav1.WorkflowStep) (*tekv1beta1.Task, errors.Error) {
	variantStep := step
	container, volumes := getTaskContainer(metadataAPIURL, taskHash, variantStep)
	return r.createTask(wr, taskHash, container, volumes)
}

func (r *Reconciler) createTask(wr *nebulav1.WorkflowRun, taskHash task.Hash, container *corev1.Container, volumes []corev1.Volume) (*tekv1beta1.Task, errors.Error) {
	taskHashKey := taskHash.HexEncoding()

	task := &tekv1beta1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskHashKey,
			Namespace: wr.GetNamespace(),
			Labels: map[string]string{
				"nebula.puppet.com/task.hash": taskHashKey,
				"nebula.puppet.com/run":       wr.GetName(),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(wr, controllerKind)},
		},
		Spec: tekv1beta1.TaskSpec{
			Steps: []tekv1beta1.Step{
				{
					Container: *container,
				},
			},
			Volumes: volumes,
		},
	}

	if err := r.createOrGetObject(context.TODO(), task); err != nil {
		return nil, errors.NewWorkflowExecutionError().WithCause(err)
	}

	return task, nil
}

func (r *Reconciler) createPipeline(wr *nebulav1.WorkflowRun, pipelineTasks []tekv1beta1.PipelineTask) (*tekv1beta1.Pipeline, errors.Error) {
	pipeline := &tekv1beta1.Pipeline{
		ObjectMeta: metav1.ObjectMeta{
			Name:            wr.GetName(),
			Namespace:       wr.GetNamespace(),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(wr, controllerKind)},
		},
		Spec: tekv1beta1.PipelineSpec{
			Tasks: pipelineTasks,
		},
	}

	if err := r.createOrGetObject(context.TODO(), pipeline); err != nil {
		return nil, errors.NewWorkflowExecutionError().WithCause(err)
	}

	return pipeline, nil
}

func (r *Reconciler) createConfigMap(wr *nebulav1.WorkflowRun, taskHash task.Hash, data map[string]string) (*corev1.ConfigMap, error) {
	taskHashKey := taskHash.HexEncoding()

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            taskHashKey,
			Namespace:       wr.GetNamespace(),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(wr, controllerKind)},
		},
		Data: data,
	}

	if err := r.createOrGetObject(context.TODO(), configMap); err != nil {
		return nil, errors.NewWorkflowExecutionError().WithCause(err)
	}

	return configMap, nil
}

func (r *Reconciler) copyImagePullSecret(wfr *nebulav1.WorkflowRun) (*corev1.Secret, error) {
	if r.Config.ImagePullSecret == "" {
		return nil, nil
	}

	klog.Infof("copying secret for system images %s", wfr.GetName())

	namespace, name, err := cache.SplitMetaNamespaceKey(r.Config.ImagePullSecret)
	if err != nil {
		return nil, err
	} else if namespace == "" {
		namespace = r.Config.Namespace
	}

	var ref corev1.Secret
	if err := r.Client.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: name}, &ref); err != nil {
		return nil, err
	}

	if ref.Type != corev1.SecretType("kubernetes.io/dockerconfigjson") {
		klog.Warningf("image pull secret is not of type kubernetes.io/dockerconfigjson")
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      imagePullSecretName,
			Namespace: wfr.GetNamespace(),
			Labels:    getLabels(wfr, nil),
		},
		Type: ref.Type,
		Data: ref.Data,
	}

	if err := r.createOrGetObject(context.TODO(), secret); err != nil {
		return nil, err
	}

	return secret, nil
}

func (r *Reconciler) createServiceAccount(wfr *nebulav1.WorkflowRun, identifier string, imagePullSecret *corev1.Secret) (*corev1.ServiceAccount, error) {
	name := strings.Join([]string{wfr.Spec.Workflow.Name, identifier}, "-")

	saccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: wfr.GetNamespace(),
			Labels:    getLabels(wfr, nil),
		},
	}

	if imagePullSecret != nil {
		saccount.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: imagePullSecret.GetName()},
		}
	}

	klog.Infof("creating service account %s", name)
	if err := r.createOrGetObject(context.TODO(), saccount); err != nil {
		return nil, err
	}

	return saccount, nil
}

func (r *Reconciler) createOrGetObject(ctx context.Context, obj runtime.Object) error {
	if err := r.Client.Create(ctx, obj); k8serrors.IsAlreadyExists(err) {
		key, err := client.ObjectKeyFromObject(obj)
		if err != nil {
			return err
		}

		if err := r.Client.Get(ctx, key, obj); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	return nil
}

func getTaskContainer(metadataAPIURL string, taskHash task.Hash, step *nebulav1.WorkflowStep) (*corev1.Container, []corev1.Volume) {
	volumeMounts := getVolumeMounts(taskHash, step)
	volumes := getVolumes(volumeMounts)
	environmentVariables := buildEnvironmentVariables(metadataAPIURL)

	image := step.Image
	if image == "" {
		image = "alpine:latest"
	}
	container := getContainer(taskHash, image, volumeMounts, environmentVariables)

	if len(step.Input) > 0 {
		container.Command = []string{NebulaMountPath + "/" + NebulaEntrypointFile}
	} else {
		if len(step.Command) > 0 {
			container.Command = []string{step.Command}
		}
		if len(step.Args) > 0 {
			container.Args = step.Args
		}
	}

	return container, volumes
}

func getContainer(taskHash task.Hash, image string, volumeMounts []corev1.VolumeMount, environmentVariables []corev1.EnvVar) *corev1.Container {
	container := &corev1.Container{
		Name:            taskHash.HexEncoding(),
		Image:           image,
		ImagePullPolicy: corev1.PullAlways,
		VolumeMounts:    volumeMounts,
		Env:             environmentVariables,
		SecurityContext: &corev1.SecurityContext{
			// We can't use RunAsUser et al. here because they don't allow write
			// access to the container filesystem. Eventually, we'll use gVisor
			// to protect us here.
			AllowPrivilegeEscalation: func(b bool) *bool { return &b }(false),
		},
	}

	return container
}

func buildEnvironmentVariables(metadataAPIURL string) []corev1.EnvVar {
	// this sets the endpoint to the metadata service for accessing the spec
	specPath := path.Join("/", "specs")

	containerVars := []corev1.EnvVar{
		{
			// TODO SPEC_URL will change to something else at a later date.
			// This will more than likely become a constant in the nebula-tasks package.
			Name:  "SPEC_URL",
			Value: metadataAPIURL + specPath,
		},
		{
			Name:  "METADATA_API_URL",
			Value: metadataAPIURL,
		},
	}

	return containerVars
}

func getVolumes(volumeMounts []corev1.VolumeMount) []corev1.Volume {
	volumes := make([]corev1.Volume, 0)

	knownVolumes := make(map[string]bool)

	defaultMode := int32(0700)
	for _, volume := range volumeMounts {
		volumeName := volume.Name

		if !knownVolumes[volumeName] {
			thisVolume := corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: volumeName,
						},
						DefaultMode: &defaultMode,
					},
				},
			}

			knownVolumes[volumeName] = true
			volumes = append(volumes, thisVolume)
		}
	}

	return volumes
}

func getVolumeMounts(taskHash task.Hash, step *nebulav1.WorkflowStep) []corev1.VolumeMount {
	volumeMounts := make([]corev1.VolumeMount, 0)

	taskHashKey := taskHash.HexEncoding()

	if len(step.Spec) > 0 {
		thisContainerMount := corev1.VolumeMount{
			Name:      taskHashKey,
			MountPath: NebulaMountPath + "/" + NebulaSpecFile,
			SubPath:   NebulaSpecFile,
		}

		volumeMounts = append(volumeMounts, thisContainerMount)
	}

	if len(step.Input) > 0 {
		thisContainerMount := corev1.VolumeMount{
			Name:      taskHashKey,
			MountPath: NebulaMountPath + "/" + NebulaEntrypointFile,
			SubPath:   NebulaEntrypointFile,
		}

		volumeMounts = append(volumeMounts, thisContainerMount)
	}

	return volumeMounts
}

func getConfigMapData(workflowParameters, workflowRunParameters relayv1beta1.UnstructuredObject, step *nebulav1.WorkflowStep) (map[string]string, errors.Error) {
	configMapData := make(map[string]string)

	ev := evaluate.NewEvaluator(
		evaluate.WithResultMapper(evaluate.NewJSONResultMapper()),
		evaluate.WithParameterTypeResolver(resolve.ParameterTypeResolverFunc(func(ctx context.Context, name string) (interface{}, error) {
			if p, ok := workflowRunParameters[name]; ok {
				return p.Value(), nil
			} else if p, ok := workflowParameters[name]; ok {
				return p.Value(), nil
			}

			return nil, &resolve.ParameterNotFoundError{Name: name}
		})),
	)

	if len(step.Spec) > 0 {
		// Inject parameters.
		r, err := ev.EvaluateAll(context.TODO(), parse.Tree(step.Spec.Value()))
		if err != nil {
			return nil, errors.NewTaskSpecEvaluationError().WithCause(err)
		}

		configMapData[NebulaSpecFile] = string(r.Value.([]byte))
	}

	if len(step.Input) > 0 {
		entrypoint := strings.Join(step.Input, "\n")

		if !strings.HasPrefix(entrypoint, InterpreterDirective) {
			entrypoint = InterpreterDefault + "\n" + entrypoint
		}

		configMapData[NebulaEntrypointFile] = entrypoint
	}

	if when := step.When.Value(); when != nil {
		r, err := ev.EvaluateAll(context.TODO(), parse.Tree(when))
		if err != nil {
			return nil, errors.NewTaskSpecEvaluationError().WithCause(err)
		}

		configMapData[NebulaConditionalsKey] = string(r.Value.([]byte))
	}

	return configMapData, nil
}

func extractPodAndTaskNamesFromPipelineRun(plr *tekv1beta1.PipelineRun) []podAndTaskName {
	var result []podAndTaskName
	for _, taskRun := range plr.Status.TaskRuns {
		if nil == taskRun {
			continue
		}
		if nil == taskRun.Status {
			continue
		}
		// Ensure the pod got initialized:
		init := false
		for _, step := range taskRun.Status.Steps {
			if step.Name != taskRun.PipelineTaskName {
				continue
			}
			if nil != step.Terminated || nil != step.Running {
				init = true
			}
		}
		if !init {
			continue
		}
		result = append(result, podAndTaskName{
			PodName:  taskRun.Status.PodName,
			TaskName: taskRun.PipelineTaskName,
		})
	}
	return result
}

func areWeDoneYet(plr *tekv1beta1.PipelineRun) bool {
	if !plr.IsDone() && !plr.IsCancelled() && !plr.IsTimedOut() {
		return false
	}

	for _, task := range plr.Status.TaskRuns {
		if task.Status == nil {
			continue
		}

		status := mapStatus(task.Status.Status)
		if status == WorkflowRunStatusInProgress {
			return false
		}
	}

	return true
}

func isCancelled(wr *nebulav1.WorkflowRun) bool {
	cancelled := false
	workflowState := wr.State.Workflow
	if cancelState, ok := workflowState[WorkflowRunStateCancel]; ok {
		cancelled, ok = cancelState.Value().(bool)
	}

	return cancelled
}

func getLabels(wfr *nebulav1.WorkflowRun, additional map[string]string) map[string]string {
	workflowRunLabels := map[string]string{
		workflowRunLabel: wfr.Spec.Name,
		workflowLabel:    wfr.Spec.Workflow.Name,
	}

	if additional != nil {
		for k, v := range additional {
			workflowRunLabels[k] = v
		}
	}

	return workflowRunLabels
}

func mapStatus(status duckv1beta1.Status) WorkflowRunStatus {
	for _, cs := range status.Conditions {
		switch cs.Type {
		case apis.ConditionSucceeded:
			switch cs.Status {
			case corev1.ConditionUnknown:
				return WorkflowRunStatusInProgress
			case corev1.ConditionTrue:
				return WorkflowRunStatusSuccess
			case corev1.ConditionFalse:
				if cs.Reason == ReasonConditionCheckFailed {
					return WorkflowRunStatusSkipped
				}
				if cs.Reason == ReasonTimedOut {
					return WorkflowRunStatusTimedOut
				}
				return WorkflowRunStatusFailure
			}
		}
	}

	return WorkflowRunStatusPending
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}
