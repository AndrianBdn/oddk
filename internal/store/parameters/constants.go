package parameters

const DefaultParameterGroup = "default:2025-08-27"

// GetDefaultParameters returns the default parameter configuration for PostgreSQL instances.
// This function acts as a constant by always returning the same configuration.
func GetDefaultParameters() []Parameter {
	return []Parameter{
		{
			Name:      "shared_buffers",
			Type:      "postgres_cli_arg",
			ValueType: "numeric_mem",
			Value:     "{expr}round(DBContainerMemoryMB / 4){/expr} MB",
		},
		{
			Name:      "max_connections",
			Type:      "postgres_cli_arg",
			ValueType: "numeric",
			Value:     "{expr}round(DBContainerMemoryMB / 16){/expr}",
		},
		{
			Name:      "effective_cache_size",
			Type:      "postgres_cli_arg",
			ValueType: "numeric_mem",
			Value:     "{expr}round(DBContainerMemoryMB * 0.75){/expr} MB",
		},
		{
			Name:      "effective_io_concurrency",
			Type:      "postgres_cli_arg",
			ValueType: "numeric",
			Value:     "{expr}DBContainerCoreCount * 8{/expr}",
		},
		{
			Name:      "max_worker_processes",
			Type:      "postgres_cli_arg",
			ValueType: "numeric",
			Value:     "{expr}max(8, DBContainerCoreCount * 2){/expr}",
		},
		{
			Name:      "maintenance_work_mem",
			Type:      "postgres_cli_arg",
			ValueType: "numeric_mem",
			Value:     "{expr}min(max(round(DBContainerMemoryMB * 0.05), 64), 2048){/expr} MB",
		},
		{
			Name:      "work_mem",
			Type:      "postgres_cli_arg",
			ValueType: "numeric_mem",
			Value:     "{expr}min(max(round((DBContainerMemoryMB * 0.25) / MaxConnections), 4), 64){/expr} MB",
		},
	}
}
