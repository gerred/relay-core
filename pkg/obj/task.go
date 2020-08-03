package obj

import (
	"context"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	nebulav1 "github.com/puppetlabs/relay-core/pkg/apis/nebula.puppet.com/v1"
	"github.com/puppetlabs/relay-core/pkg/model"
	tektonv1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Task struct {
	Key    client.ObjectKey
	Object *tektonv1beta1.Task
}

var _ Persister = &Task{}
var _ Loader = &Task{}
var _ Ownable = &Task{}

func (t *Task) Persist(ctx context.Context, cl client.Client) error {
	return CreateOrUpdate(ctx, cl, t.Key, t.Object)
}

func (t *Task) Load(ctx context.Context, cl client.Client) (bool, error) {
	return GetIgnoreNotFound(ctx, cl, t.Key, t.Object)
}

func (t *Task) Owned(ctx context.Context, owner Owner) error {
	return Own(t.Object, owner)
}

func NewTask(key client.ObjectKey) *Task {
	return &Task{
		Key:    key,
		Object: &tektonv1beta1.Task{},
	}
}

func ConfigureTask(ctx context.Context, t *Task, wrd *WorkflowRunDeps, ws *nebulav1.WorkflowStep) error {
	image := ws.Image
	if image == "" {
		image = model.DefaultImage
	}

	if wrd.WorkflowRun.Object.Spec.TenantRef != nil {
		ref, err := name.ParseReference(ws.Image, name.WeakValidation)
		if err != nil {
			return err
		}

		img, err := remote.Image(ref)
		if err != nil {
			return err
		}

		ep, cmd, _, err := imageData(ref, img)
		if err != nil {
			return err
		}

		var args []string
		var argsForEntrypoint []string

		if len(ep) > 0 {
			argsForEntrypoint = append(argsForEntrypoint, "-entrypoint", ep[0], "--")
			args = append(ep[1:], args...)
			args = append(cmd[0:], args...)
			argsForEntrypoint = append(argsForEntrypoint, args...)
		} else {
			argsForEntrypoint = append(argsForEntrypoint, "-entrypoint", cmd[0], "--")
			args = append(cmd[1:], args...)
			argsForEntrypoint = append(argsForEntrypoint, args...)
		}

		step := tektonv1beta1.Step{
			Container: corev1.Container{
				Name:            "step",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways,
				Command:         []string{"/data/entrypoint"},
				Args:            argsForEntrypoint,
				Env: []corev1.EnvVar{
					{
						Name:  "METADATA_API_URL",
						Value: wrd.MetadataAPIURL.String(),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					// We can't use RunAsUser et al. here because they don't allow write
					// access to the container filesystem. Eventually, we'll use gVisor
					// to protect us here.
					AllowPrivilegeEscalation: func(b bool) *bool { return &b }(false),
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "entrypoint",
						MountPath: "/data",
						ReadOnly:  true,
					},
				},
			},
		}

		if err := wrd.AnnotateStepToken(ctx, &t.Object.ObjectMeta, ws); err != nil {
			return err
		}

		t.Object.Spec.Steps = []tektonv1beta1.Step{step}

		claim := wrd.WorkflowRun.Object.Spec.TenantRef.Name + "-volume-rox"
		t.Object.Spec.Volumes = []corev1.Volume{
			{
				Name: "entrypoint",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claim,
						ReadOnly:  true,
					},
				},
			},
		}
	} else {

		step := tektonv1beta1.Step{
			Container: corev1.Container{
				Name:            "step",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways,
				Env: []corev1.EnvVar{
					{
						Name:  "METADATA_API_URL",
						Value: wrd.MetadataAPIURL.String(),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					// We can't use RunAsUser et al. here because they don't allow write
					// access to the container filesystem. Eventually, we'll use gVisor
					// to protect us here.
					AllowPrivilegeEscalation: func(b bool) *bool { return &b }(false),
				},
			},
		}

		if len(ws.Input) > 0 {
			step.Script = model.ScriptForInput(ws.Input)
		} else {
			if len(ws.Command) > 0 {
				step.Container.Command = []string{ws.Command}
			}

			if len(ws.Args) > 0 {
				step.Container.Args = ws.Args
			}
		}

		if err := wrd.AnnotateStepToken(ctx, &t.Object.ObjectMeta, ws); err != nil {
			return err
		}

		t.Object.Spec.Steps = []tektonv1beta1.Step{step}
	}

	return nil
}

type Tasks struct {
	Deps *WorkflowRunDeps
	List []*Task
}

var _ Persister = &Tasks{}
var _ Loader = &Tasks{}
var _ Ownable = &Tasks{}

func (ts *Tasks) Persist(ctx context.Context, cl client.Client) error {
	for _, t := range ts.List {
		if err := t.Persist(ctx, cl); err != nil {
			return err
		}
	}

	return nil
}

func (ts *Tasks) Load(ctx context.Context, cl client.Client) (bool, error) {
	all := true

	for _, t := range ts.List {
		ok, err := t.Load(ctx, cl)
		if err != nil {
			return false, err
		} else if !ok {
			all = false
		}
	}

	return all, nil
}

func (ts *Tasks) Owned(ctx context.Context, owner Owner) error {
	for _, t := range ts.List {
		if err := t.Owned(ctx, owner); err != nil {
			return err
		}
	}

	return nil
}

func NewTasks(wrd *WorkflowRunDeps) *Tasks {
	ts := &Tasks{
		Deps: wrd,
		List: make([]*Task, len(wrd.WorkflowRun.Object.Spec.Workflow.Steps)),
	}

	for i, ws := range wrd.WorkflowRun.Object.Spec.Workflow.Steps {
		ts.List[i] = NewTask(ModelStepObjectKey(wrd.WorkflowRun.Key, ModelStep(wrd.WorkflowRun, ws)))
	}

	return ts
}

func ConfigureTasks(ctx context.Context, ts *Tasks) error {
	if err := ts.Deps.WorkflowRun.Own(ctx, ts); err != nil {
		return err
	}

	for i, ws := range ts.Deps.WorkflowRun.Object.Spec.Workflow.Steps {
		if err := ConfigureTask(ctx, ts.List[i], ts.Deps, ws); err != nil {
			return err
		}
	}

	return nil
}

func imageData(ref name.Reference, img v1.Image) ([]string, []string, name.Digest, error) {
	digest, err := img.Digest()
	if err != nil {
		return nil, nil, name.Digest{}, err
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, nil, name.Digest{}, err
	}

	d, err := name.NewDigest(ref.Context().String()+"@"+digest.String(), name.WeakValidation)
	if err != nil {
		return nil, nil, name.Digest{}, err
	}

	return cfg.Config.Entrypoint, cfg.Config.Cmd, d, nil
}
