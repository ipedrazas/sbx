package models

import "encoding/json"

// UnmarshalJSON applies the serde defaults (`searchable`, `sortable` and
// `visible`-adjacent flags default to true, `pii_policy` to "none",
// `field_type` to "string") when keys are absent from the input.
func (f *DatasetField) UnmarshalJSON(b []byte) error {
	type plain DatasetField
	p := plain(NewDatasetField())
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*f = DatasetField(p)
	return nil
}

// UnmarshalJSON defaults risk_level to medium and required_scopes to [] when absent.
func (a *Action) UnmarshalJSON(b []byte) error {
	type plain Action
	p := plain{RiskLevel: RiskMedium, RequiredScopes: []string{}}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*a = Action(p)
	return nil
}
