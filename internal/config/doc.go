// Package config turns environment variables into a typed Config with
// defaults and mode-specific validation.
//
// Where it's used: every binary calls Load at startup, then the validator
// matching its run mode: -migrate -> ValidateForMigrate, -seed ->
// ValidateForSeed, serve -> ValidateForServe.
//
// Details: values are read with stdlib os only (no dotenv library);
// empty-string values count as unset so defaults apply; validation is split
// per mode so one-off operations do not fail on unrelated runtime variables.
package config
