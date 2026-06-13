package instances

import (
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/hypersequent/oddk/internal/operr"
	"github.com/hypersequent/oddk/internal/rfc3339time"
)

type InstanceStore struct {
	db *sqlx.DB
}

func NewInstanceStore(db *sqlx.DB) *InstanceStore {
	return &InstanceStore{db: db}
}

func (s *InstanceStore) Create(name string, port int, version, password, containerID string, cpuCores, ramMB int, parameterGroup, image string) (*RDBMSInstance, error) {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`
		INSERT INTO rdbms_instances (name, port, version, status, container_id, password, cpu_cores, ram_mb, parameter_group, image, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, name, port, version, "creating", containerID, password, cpuCores, ramMB, parameterGroup, image, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert rdbms instance: %w", err)
	}

	_, err = result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}

	return s.Get(name)
}

func (s *InstanceStore) Get(name string) (*RDBMSInstance, error) {
	var instance RDBMSInstance
	err := s.db.Get(&instance, "SELECT * FROM rdbms_instances WHERE name = ?", name)
	if err == sql.ErrNoRows {
		return nil, operr.NotFoundf("instance not found: %s", name)
	}
	if err != nil {
		return nil, fmt.Errorf("get rdbms instance: %w", err)
	}
	return &instance, nil
}

func (s *InstanceStore) List() ([]RDBMSInstance, error) {
	var instances []RDBMSInstance
	err := s.db.Select(&instances, `SELECT * FROM rdbms_instances`)
	if err != nil {
		return nil, fmt.Errorf("list rdbms instances: %w", err)
	}
	return instances, nil
}

func (s *InstanceStore) UpdateStatus(name, status string) error {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`UPDATE rdbms_instances SET status = ?, updated_at = ? WHERE name = ?`,
		status, now, name)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("instance %s not found", name)
	}
	return nil
}

func (s *InstanceStore) UpdateContainerID(name, containerID string) error {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`UPDATE rdbms_instances SET container_id = ?, updated_at = ? WHERE name = ?`,
		containerID, now, name)
	if err != nil {
		return fmt.Errorf("update container ID: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("instance %s not found", name)
	}
	return nil
}

func (s *InstanceStore) UpdatePassword(name, encryptedPassword string) error {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`UPDATE rdbms_instances SET password = ?, updated_at = ? WHERE name = ?`,
		encryptedPassword, now, name)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("instance %s not found", name)
	}
	return nil
}

func (s *InstanceStore) UpdateImage(name, image, version string) error {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`UPDATE rdbms_instances SET image = ?, version = ?, updated_at = ? WHERE name = ?`,
		image, version, now, name)
	if err != nil {
		return fmt.Errorf("update image: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("instance %s not found", name)
	}
	return nil
}

func (s *InstanceStore) UpdateParameterGroup(name, parameterGroup string) error {
	now := rfc3339time.Now()
	result, err := s.db.Exec(`UPDATE rdbms_instances SET parameter_group = ?, updated_at = ? WHERE name = ?`,
		parameterGroup, now, name)
	if err != nil {
		return fmt.Errorf("update parameter group: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("instance %s not found", name)
	}
	return nil
}

func (s *InstanceStore) Delete(name string) error {
	_, err := s.db.Exec(`DELETE FROM rdbms_instances WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete rdbms instance: %w", err)
	}
	return nil
}

func (s *InstanceStore) IsPortInUse(port int) (bool, string, error) {
	var name sql.NullString
	err := s.db.Get(&name, `SELECT name FROM rdbms_instances WHERE port = ?`, port)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("check port usage: %w", err)
	}
	return true, name.String, nil
}

func (s *InstanceStore) IsNameInUse(name string) (bool, error) {
	var count int
	err := s.db.Get(&count, `SELECT COUNT(*) FROM rdbms_instances WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("check name usage: %w", err)
	}
	return count > 0, nil
}
