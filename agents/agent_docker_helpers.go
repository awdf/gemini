package agents

import (
	"encoding/json"
	"fmt"

	"google.golang.org/genai"
)

// parseJsonTools parses a JSON tool list from MCP server
func parseJsonTools(data []byte) ([]*genai.FunctionDeclaration, error) {
	var toolList []map[string]interface{}
	if err := json.Unmarshal(data, &toolList); err != nil {
		return nil, err
	}

	var functions []*genai.FunctionDeclaration
	for _, tool := range toolList {
		fn := parseJsonTool(tool)
		if fn != nil {
			functions = append(functions, fn)
		}
	}
	return functions, nil
}

// parseJsonTool parses a single tool from JSON format
func parseJsonTool(tool map[string]interface{}) *genai.FunctionDeclaration {
	name, ok := tool["name"].(string)
	if !ok {
		return nil
	}

	description, _ := tool["description"].(string)
	parameters, _ := tool["parameters"].(map[string]interface{})

	// Use the exact name from the MCP server, don't convert it
	properties, required := parseJsonParameters(parameters)

	return &genai.FunctionDeclaration{
		Name:        name, // Keep the original snake_case name
		Description: fmt.Sprintf("DOCKER: %s", description),
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: properties,
			Required:   required,
		},
	}
}

// parseJsonParameters parses parameters from JSON format
func parseJsonParameters(parameters map[string]interface{}) (map[string]*genai.Schema, []string) {
	properties := make(map[string]*genai.Schema)
	var required []string

	if params, ok := parameters["properties"].(map[string]interface{}); ok {
		for paramName, paramDef := range params {
			if def, ok := paramDef.(map[string]interface{}); ok {
				schema := parseJsonParameterSchema(def)
				properties[paramName] = schema
			}
		}

		if req, ok := parameters["required"].([]interface{}); ok {
			for _, r := range req {
				if paramName, ok := r.(string); ok {
					required = append(required, paramName)
				}
			}
		}
	}

	return properties, required
}

// parseJsonParameterSchema parses a single parameter schema
func parseJsonParameterSchema(def map[string]interface{}) *genai.Schema {
	schema := &genai.Schema{
		Description: fmt.Sprint(def["description"]),
	}

	switch def["type"] {
	case "string":
		schema.Type = genai.TypeString
	case "integer", "number":
		schema.Type = genai.TypeNumber
	case "boolean":
		schema.Type = genai.TypeBoolean
	case "array":
		schema.Type = genai.TypeArray
		if _, ok := def["items"].(map[string]interface{}); ok {
			// For array types, always use string items for now
			schema.Items = &genai.Schema{Type: genai.TypeString}
		}
	default:
		schema.Type = genai.TypeString
	}

	return schema
}