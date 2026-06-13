package parameters

type Parameter struct {
	GroupName string `db:"group_name" json:"groupName"`
	Name      string `db:"name" json:"name"`
	Type      string `db:"type" json:"type"`
	ValueType string `db:"value_type" json:"valueType"`
	Value     string `db:"value" json:"value"`
	CreatedAt string `db:"created_at" json:"createdAt"`
}

type ParameterGroup struct {
	Name       string      `json:"name"`
	Parameters []Parameter `json:"parameters"`
}

type ResolvedParameter struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ValueType string `json:"valueType"`
	Value     string `json:"value"`
}
