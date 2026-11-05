// Package plan is the model of a Norca deployment plan: the types of the
// plan (with their YAML and JSON keys; HCL uses the same keys), loading it
// from a file or from memory, the default values, and the validation.
//
// It is the model norca-deployer works with, and it can be embedded in
// other programs: an operator, a user interface or another tool can read,
// check and write plans without the deployer, which adds the installation
// (SSH, packages, the agents) on top of it. The package has no state of its
// own and makes no connection; besides the standard library it depends on
// the YAML and HCL parsers and on norca-pki (the trust domain of the plan).
//
//	p, err := plan.Parse(data, plan.FormatYAML, "")
//	if err != nil {
//		// a syntax error or the first rule the plan breaks
//	}
//	fmt.Println(p.Metadata.Region, len(p.Inventory.Masters))
//
// Parse and LoadFromFile apply the defaults (SetDefaults) and validate
// (Validate); a plan built in code is checked the same way by calling the
// two. SaveToFile writes a plan as YAML.
package plan
