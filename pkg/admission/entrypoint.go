package admission

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/puppetlabs/relay-core/pkg/model"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type EntrypointHandler struct {
	decoder *admission.Decoder
}

var _ admission.Handler = &EntrypointHandler{}
var _ admission.DecoderInjector = &EntrypointHandler{}

func (eh *EntrypointHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := eh.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if claim, ok := pod.ObjectMeta.GetAnnotations()[model.RelayControllerVolumeClaimAnnotation]; ok {
		cs := make([]corev1.Container, 0)

		updated := false
		for _, c := range pod.Spec.Containers {
			hasVolumeMount := false
			for _, vm := range c.VolumeMounts {
				if vm.Name == model.EntrypointVolumeMountName {
					hasVolumeMount = true
					break
				}
			}

			if !hasVolumeMount {
				c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
					Name:      model.EntrypointVolumeMountName,
					MountPath: model.EntrypointVolumeMountPath,
					ReadOnly:  true,
				})

				updated = true
			}

			cs = append(cs, c)
		}

		if updated {
			pod.Spec.Containers = cs
		}

		hasVolume := false
		for _, volume := range pod.Spec.Volumes {
			if volume.Name == model.EntrypointVolumeMountName {
				hasVolume = true
				break
			}
		}

		if !hasVolume {
			if pod.Spec.Volumes == nil {
				pod.Spec.Volumes = make([]corev1.Volume, 0)
			}

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: model.EntrypointVolumeMountName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claim,
						ReadOnly:  true,
					},
				},
			})

			updated = true
		}

		if updated {
			b, err := json.Marshal(pod)
			if err != nil {
				return admission.Errored(http.StatusInternalServerError, err)
			}

			return admission.PatchResponseFromRaw(req.Object.Raw, b)
		}
	}

	return admission.Allowed("")
}

func (eh *EntrypointHandler) InjectDecoder(d *admission.Decoder) error {
	eh.decoder = d
	return nil
}

type EntrypointHandlerOption func(eh *EntrypointHandler)

func NewEntrypointHandler(opts ...EntrypointHandlerOption) *EntrypointHandler {
	eh := &EntrypointHandler{}

	for _, opt := range opts {
		opt(eh)
	}

	return eh
}
