package operations

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/operr"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/util"
)

// ReconfigureRDBMSParams describes an in-place change to an instance's shape:
// its parameter group, CPU allocation, RAM, port, or any combination. Every
// field is optional and an unset one keeps the current value, so moving the
// port does not require restating the RAM. At least one must be set.
type ReconfigureRDBMSParams struct {
	Name           string
	ParameterGroup string
	CPUCores       *int
	RAMMB          *int
	Port           *int
}

// ReconfigureRDBMSResult contains the result of reconfiguring an RDBMS instance
type ReconfigureRDBMSResult struct {
	Instance *instances.RDBMSInstance
}

// ReconfigureRDBMSOp rebuilds an existing instance's container with a new shape.
//
// It is the same recreate-or-roll-back path that switch and update use
// (applyClusterChange): everything that can be refused is refused before the
// running container is touched, a container that is built but never reaches
// readiness is replaced by the previous one, and the store records the new
// shape only once PostgreSQL accepts connections on it. The data volume is
// never touched. RAM is more than a cgroup limit here: parameter groups
// resolve against it, so a RAM change also re-resolves shared_buffers and
// friends, and the shared-memory fit is re-checked against the NEW size.
type ReconfigureRDBMSOp struct {
	deps   *Dependencies
	params ReconfigureRDBMSParams
	result *ReconfigureRDBMSResult
}

// NewReconfigureRDBMSOp creates a new reconfigure RDBMS operation
func NewReconfigureRDBMSOp(deps *Dependencies, params ReconfigureRDBMSParams) *ReconfigureRDBMSOp {
	return &ReconfigureRDBMSOp{
		deps:   deps,
		params: params,
	}
}

func (op *ReconfigureRDBMSOp) Name() string {
	return fmt.Sprintf("ReconfigureRDBMS[%s]", op.params.Name)
}

func (op *ReconfigureRDBMSOp) Type() OpType {
	return OpTypeWrite
}

// reconfigureTarget is the shape the instance will have after the change.
type reconfigureTarget struct {
	group    string
	cpuCores int
	ramMB    int
	port     int
}

func (t reconfigureTarget) sameAs(inst *instances.RDBMSInstance) bool {
	return t.group == inst.ParameterGroup && t.cpuCores == inst.CPUCores &&
		t.ramMB == inst.RAMMB && t.port == inst.Port
}

// describe names what changes, for messages: "parameter group X", or a list
// such as "RAM 2048 MB, port 5433".
func (t reconfigureTarget) describe(inst *instances.RDBMSInstance) string {
	var parts []string
	if t.group != inst.ParameterGroup {
		parts = append(parts, "parameter group "+t.group)
	}
	if t.cpuCores != inst.CPUCores {
		parts = append(parts, fmt.Sprintf("CPU %d cores", t.cpuCores))
	}
	if t.ramMB != inst.RAMMB {
		parts = append(parts, fmt.Sprintf("RAM %d MB", t.ramMB))
	}
	if t.port != inst.Port {
		parts = append(parts, fmt.Sprintf("port %d", t.port))
	}
	if len(parts) == 0 {
		return "the current configuration"
	}
	return strings.Join(parts, ", ")
}

// resolveTarget merges the request onto the instance's current shape and
// refuses anything that cannot work, all before the container is touched.
func (op *ReconfigureRDBMSOp) resolveTarget(inst *instances.RDBMSInstance) (reconfigureTarget, error) {
	t := reconfigureTarget{
		group:    inst.ParameterGroup,
		cpuCores: inst.CPUCores,
		ramMB:    inst.RAMMB,
		port:     inst.Port,
	}
	p := op.params
	if p.ParameterGroup == "" && p.CPUCores == nil && p.RAMMB == nil && p.Port == nil {
		return t, operr.Invalidf("nothing to change: pass a parameter group, cpuCores, ramMb and/or port")
	}
	if p.ParameterGroup != "" {
		t.group = p.ParameterGroup
	}
	if p.CPUCores != nil {
		t.cpuCores = *p.CPUCores
	}
	if p.RAMMB != nil {
		t.ramMB = *p.RAMMB
	}
	if p.CPUCores != nil || p.RAMMB != nil {
		// The same two checks create applies. Against the HOST, not just the
		// bounds: a RAM figure the machine does not have would build a container
		// PostgreSQL cannot start in, and the rollback would then be the only
		// thing keeping the instance up.
		if err := util.ValidateBasicResourceBounds(t.cpuCores, t.ramMB); err != nil {
			return t, operr.Invalidf("invalid resource values: %v", err)
		}
		if err := util.ValidateSystemResources(t.cpuCores, t.ramMB); err != nil {
			return t, operr.Invalidf("invalid resource allocation: %v", err)
		}
	}
	if p.Port != nil {
		t.port = *p.Port
		if t.port < 1 || t.port > 65535 {
			return t, operr.Invalidf("port must be between 1 and 65535")
		}
		if t.port != inst.Port {
			inUse, holder, err := op.deps.Store.Instances.IsPortInUse(t.port)
			if err != nil {
				return t, fmt.Errorf("check port availability: %w", err)
			}
			if inUse {
				return t, operr.Conflictf("port %d is already in use by instance: %s", t.port, holder)
			}
			// The instance's own port legitimately answers (its container is
			// still running); any OTHER port must be free on the host, or the
			// new container would come up unreachable and be rolled back for
			// nothing.
			if hostPortAnswers(t.port) {
				return t, operr.Conflictf("port %d is already in use on this host (something answers on %s:%d)",
					t.port, util.GatewayIP, t.port)
			}
		}
	}
	return t, nil
}

func (op *ReconfigureRDBMSOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	target, err := op.resolveTarget(instance)
	if err != nil {
		return err
	}

	// The current shape on a healthy instance is a no-op. The same shape on an
	// instance in "error" is a retry after a failed apply (the store may
	// already record the new values, or a rollback restored the old ones —
	// either way Recreate is the recovery).
	if target.sameAs(instance) && instance.Status != instances.StatusError {
		if op.params.ParameterGroup != "" && op.params.CPUCores == nil && op.params.RAMMB == nil && op.params.Port == nil {
			return operr.Invalidf("instance already uses parameter group: %s", op.params.ParameterGroup)
		}
		return operr.Invalidf("instance %s already has this configuration; nothing to change", op.params.Name)
	}

	parameterGroup, err := op.deps.Store.Parameters.GetGroup(target.group)
	if err != nil {
		return operr.Invalidf("get parameter group %s: %w", target.group, err)
	}

	prevGroup, err := op.deps.Store.Parameters.GetGroup(instance.ParameterGroup)
	if err != nil {
		return fmt.Errorf("get current parameter group %s: %w", instance.ParameterGroup, err)
	}

	password, err := crypto.DecryptPassword(instance.Password, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}

	prev := specFromInstance(instance, password, prevGroup)
	next := prev
	next.ParameterGroup = target.group
	next.Parameters = parameterGroup.Parameters
	next.CPUCores = target.cpuCores
	next.RAMMB = target.ramMB
	next.Port = target.port

	change := target.describe(instance)
	prevStatus := instance.Status
	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, instances.StatusReconfiguring); err != nil {
		log.Printf("Error updating status to reconfiguring: %v", err)
	}

	rolledBack, err := applyClusterChange(ctx, op.deps, next, prev, instance.ContainerID)
	if rolledBack {
		return fmt.Errorf("%s could not start: %w (rolled back to the previous configuration)", change, err)
	}
	if err != nil {
		if prevStatus != instances.StatusError && errors.Is(err, operr.ErrInvalid) {
			if statusErr := op.deps.Store.Instances.UpdateStatus(op.params.Name, prevStatus); statusErr != nil {
				log.Printf("Error restoring status after preflight refusal: %v", statusErr)
			}
		}
		return err
	}

	// Commit the new shape only after PostgreSQL is accepting connections on
	// it. Recording it before start meant a failed apply stored the bad group,
	// so re-applying it was refused and recovery required a *different* group.
	if target.group != instance.ParameterGroup {
		if err := op.deps.Store.Instances.UpdateParameterGroup(op.params.Name, target.group); err != nil {
			return fmt.Errorf("record parameter group: %w", err)
		}
	}
	if target.cpuCores != instance.CPUCores || target.ramMB != instance.RAMMB || target.port != instance.Port {
		if err := op.deps.Store.Instances.UpdateResources(op.params.Name, target.port, target.cpuCores, target.ramMB); err != nil {
			return fmt.Errorf("record resources: %w", err)
		}
	}
	if err := op.deps.Store.Instances.UpdateStatus(op.params.Name, instances.StatusRunning); err != nil {
		return fmt.Errorf("record running status: %w", err)
	}

	instance, err = op.deps.Store.Instances.Get(op.params.Name)
	if err != nil {
		return fmt.Errorf("get updated instance: %w", err)
	}

	op.result = &ReconfigureRDBMSResult{
		Instance: instance,
	}

	return nil
}

// GetResult returns the operation result
func (op *ReconfigureRDBMSOp) GetResult() *ReconfigureRDBMSResult {
	return op.result
}
