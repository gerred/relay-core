package builder

import (
	"github.com/puppetlabs/nebula-tasks/pkg/manager/reject"
	"github.com/puppetlabs/nebula-tasks/pkg/model"
)

type metadataManagers struct {
	conditions  model.ConditionGetterManager
	secrets     model.SecretManager
	spec        model.SpecGetterManager
	state       model.StateGetterManager
	stepOutputs model.StepOutputManager
}

var _ model.MetadataManagers = &metadataManagers{}

func (mm *metadataManagers) Conditions() model.ConditionGetterManager {
	return mm.conditions
}

func (mm *metadataManagers) Secrets() model.SecretManager {
	return mm.secrets
}

func (mm *metadataManagers) Spec() model.SpecGetterManager {
	return mm.spec
}

func (mm *metadataManagers) State() model.StateGetterManager {
	return mm.state
}

func (mm *metadataManagers) StepOutputs() model.StepOutputManager {
	return mm.stepOutputs
}

type MetadataBuilder struct {
	conditions  model.ConditionGetterManager
	secrets     model.SecretManager
	spec        model.SpecGetterManager
	state       model.StateGetterManager
	stepOutputs model.StepOutputManager
}

func (mb *MetadataBuilder) SetConditions(m model.ConditionGetterManager) *MetadataBuilder {
	mb.conditions = m
	return mb
}

func (mb *MetadataBuilder) SetSecrets(m model.SecretManager) *MetadataBuilder {
	mb.secrets = m
	return mb
}

func (mb *MetadataBuilder) SetSpec(m model.SpecGetterManager) *MetadataBuilder {
	mb.spec = m
	return mb
}

func (mb *MetadataBuilder) SetState(m model.StateGetterManager) *MetadataBuilder {
	mb.state = m
	return mb
}

func (mb *MetadataBuilder) SetStepOutputs(m model.StepOutputManager) *MetadataBuilder {
	mb.stepOutputs = m
	return mb
}

func (mb *MetadataBuilder) Build() model.MetadataManagers {
	return &metadataManagers{
		conditions:  mb.conditions,
		secrets:     mb.secrets,
		spec:        mb.spec,
		state:       mb.state,
		stepOutputs: mb.stepOutputs,
	}
}

func NewMetadataBuilder() *MetadataBuilder {
	return &MetadataBuilder{
		conditions:  reject.ConditionManager,
		secrets:     reject.SecretManager,
		spec:        reject.SpecManager,
		state:       reject.StateManager,
		stepOutputs: reject.StepOutputManager,
	}
}
