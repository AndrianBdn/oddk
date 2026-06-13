package parameters

import (
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

type ParameterStore struct {
	db *sqlx.DB
}

func NewParameterStore(db *sqlx.DB) *ParameterStore {
	return &ParameterStore{db: db}
}

func (s *ParameterStore) CreateGroup(groupName string, parameters []Parameter) error {
	if groupName == "" {
		return fmt.Errorf("group name cannot be empty")
	}

	// Prevent creating parameter groups with "default:" prefix (except during initialization)
	if strings.HasPrefix(groupName, "default:") && groupName != DefaultParameterGroup {
		return fmt.Errorf("cannot create parameter groups with 'default:' prefix")
	}

	// Check if group already exists
	exists, err := s.GroupExists(groupName)
	if err != nil {
		return fmt.Errorf("check group existence: %w", err)
	}
	if exists {
		return fmt.Errorf("parameter group '%s' already exists", groupName)
	}

	now := time.Now().Format(time.RFC3339)

	tx, err := s.db.Beginx()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback() // Ignore error in defer
	}()

	for _, param := range parameters {
		_, err = tx.Exec(`
			INSERT INTO parameters (group_name, name, type, value_type, value, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, groupName, param.Name, param.Type, param.ValueType, param.Value, now)
		if err != nil {
			return fmt.Errorf("insert parameter %s: %w", param.Name, err)
		}
	}

	return tx.Commit()
}

func (s *ParameterStore) GetGroup(groupName string) (*ParameterGroup, error) {
	var parameters []Parameter
	err := s.db.Select(&parameters, `SELECT * FROM parameters WHERE group_name = ? ORDER BY name`, groupName)
	if err != nil {
		return nil, fmt.Errorf("get parameters: %w", err)
	}

	if len(parameters) == 0 {
		return nil, fmt.Errorf("parameter group not found: %s", groupName)
	}

	return &ParameterGroup{
		Name:       groupName,
		Parameters: parameters,
	}, nil
}

func (s *ParameterStore) ListGroups() ([]string, error) {
	var groups []string
	err := s.db.Select(&groups, `SELECT DISTINCT group_name FROM parameters ORDER BY group_name`)
	if err != nil {
		return nil, fmt.Errorf("list parameter groups: %w", err)
	}
	return groups, nil
}

func (s *ParameterStore) DeleteGroup(groupName string) error {
	if groupName == "" {
		return fmt.Errorf("group name cannot be empty")
	}

	// Prevent deleting the current version's default parameter group
	if groupName == DefaultParameterGroup {
		return fmt.Errorf("cannot delete the default parameter group of the current version")
	}

	// Check if any instances are using this parameter group
	var instanceCount int
	err := s.db.Get(&instanceCount, `SELECT COUNT(*) FROM rdbms_instances WHERE parameter_group = ?`, groupName)
	if err != nil {
		return fmt.Errorf("check parameter group usage: %w", err)
	}

	if instanceCount > 0 {
		return fmt.Errorf("cannot delete parameter group '%s': it is being used by %d instance(s)", groupName, instanceCount)
	}

	result, err := s.db.Exec(`DELETE FROM parameters WHERE group_name = ?`, groupName)
	if err != nil {
		return fmt.Errorf("delete parameter group: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("parameter group not found: %s", groupName)
	}

	return nil
}

func (s *ParameterStore) GroupExists(groupName string) (bool, error) {
	var count int
	err := s.db.Get(&count, `SELECT COUNT(DISTINCT group_name) FROM parameters WHERE group_name = ?`, groupName)
	if err != nil {
		return false, fmt.Errorf("check group existence: %w", err)
	}
	return count > 0, nil
}

func (s *ParameterStore) EnsureDefaultParameterGroup() error {
	exists, err := s.GroupExists(DefaultParameterGroup)
	if err != nil {
		return fmt.Errorf("check default parameter group: %w", err)
	}

	if exists {
		return nil
	}

	return s.CreateGroup(DefaultParameterGroup, GetDefaultParameters())
}
