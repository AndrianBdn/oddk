package operations

import (
	"context"
	"fmt"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/operr"
)

// SetPasswordOp is an operation to set a new password for an instance
type SetPasswordOp struct {
	deps     *Dependencies
	name     string
	password string
	result   *SetPasswordResult
}

type SetPasswordResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// NewSetPasswordOp creates a new operation to set password
func NewSetPasswordOp(deps *Dependencies, name, password string) *SetPasswordOp {
	return &SetPasswordOp{
		deps:     deps,
		name:     name,
		password: password,
	}
}

func (op *SetPasswordOp) Name() string {
	return fmt.Sprintf("set password for instance %s", op.name)
}

func (op *SetPasswordOp) Type() OpType {
	return OpTypeWrite
}

func (op *SetPasswordOp) Execute(ctx context.Context) error {
	instance, err := op.deps.Store.Instances.Get(op.name)
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}

	if instance.Status != "running" {
		return operr.Invalidf("instance %s is not running (status: %s)", op.name, instance.Status)
	}

	// Use the helper function to test connection with new password
	if err := TestPostgreSQLConnectivityWithPassword(ctx, instance.Port, op.password); err != nil {
		return fmt.Errorf("cannot set password: %w", err)
	}

	// Connection successful, now encrypt and save the password
	encryptedPassword, err := crypto.EncryptPassword(op.password, op.deps.MasterKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt password: %w", err)
	}

	if err := op.deps.Store.Instances.UpdatePassword(op.name, encryptedPassword); err != nil {
		return fmt.Errorf("failed to update password in database: %w", err)
	}

	op.result = &SetPasswordResult{
		Success: true,
		Message: fmt.Sprintf("Password updated successfully for instance %s", op.name),
	}

	return nil
}

func (op *SetPasswordOp) GetResult() *SetPasswordResult {
	return op.result
}
