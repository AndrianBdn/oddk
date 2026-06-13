package operations

import (
	"context"
	"fmt"
)

// GetPasswordOp is an operation to get the decrypted password for an instance
type GetPasswordOp struct {
	deps   *Dependencies
	name   string
	result *GetPasswordResult
}

type GetPasswordResult struct {
	Password string `json:"password"`
}

// NewGetPasswordOp creates a new operation to get password
func NewGetPasswordOp(deps *Dependencies, name string) *GetPasswordOp {
	return &GetPasswordOp{
		deps: deps,
		name: name,
	}
}

func (op *GetPasswordOp) Name() string {
	return fmt.Sprintf("get password for instance %s", op.name)
}

func (op *GetPasswordOp) Type() OpType {
	return OpTypeRead
}

func (op *GetPasswordOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.name)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}

	if instance.Status == "broken" {
		return fmt.Errorf("instance %s is broken - container may be missing", op.name)
	}

	password, err := op.deps.Store.Instances.GetDecryptedPassword(op.name, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("failed to decrypt password: %w", err)
	}

	op.result = &GetPasswordResult{
		Password: password,
	}

	return nil
}

func (op *GetPasswordOp) GetResult() *GetPasswordResult {
	return op.result
}
