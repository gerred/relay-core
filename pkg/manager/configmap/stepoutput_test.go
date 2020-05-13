package configmap_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/puppetlabs/nebula-tasks/pkg/manager/configmap"
	"github.com/puppetlabs/nebula-tasks/pkg/model"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestStepOutputManager(t *testing.T) {
	ctx := context.Background()

	step1 := &model.Step{
		Run:  model.Run{ID: "foo"},
		Name: "bar",
	}
	step2 := &model.Step{
		Run:  model.Run{ID: "foo"},
		Name: "baz",
	}

	obj := &corev1.ConfigMap{}
	om1 := configmap.NewStepOutputManager(step1, configmap.NewLocalConfigMap(obj))
	om2 := configmap.NewStepOutputManager(step2, configmap.NewLocalConfigMap(obj))

	_, err := om1.Set(ctx, "key-a", "value-a-step-1")
	require.NoError(t, err)

	_, err = om2.Set(ctx, "key-a", "value-a-step-2")
	require.NoError(t, err)

	_, err = om1.Set(ctx, "key-b", "value-b-step-1")
	require.NoError(t, err)

	for i, om := range []model.StepOutputManager{om1, om2} {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			out, err := om.Get(ctx, step1.Name, "key-a")
			require.NoError(t, err)
			require.Equal(t, "value-a-step-1", out.Value)

			out, err = om.Get(ctx, step1.Name, "key-b")
			require.NoError(t, err)
			require.Equal(t, "value-b-step-1", out.Value)

			out, err = om.Get(ctx, step2.Name, "key-a")
			require.NoError(t, err)
			require.Equal(t, "value-a-step-2", out.Value)

			_, err = om.Get(ctx, step2.Name, "key-b")
			require.Equal(t, model.ErrNotFound, err)
		})
	}
}
