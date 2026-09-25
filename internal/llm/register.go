package llm

import (
	"context"
	"fmt"
)

type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type ToolFunc func(ctx context.Context, arguments string) (string, error)

type Tool struct {
	Schema ToolSchema
	Run    ToolFunc // 统一的 run 运行
}

// ToolHandle 是工具的集合，用于管理工具的注册和运行。 set 注册
type ToolHandle struct {
	tools map[string]Tool
	order []string
}

func NewToolHandle() *ToolHandle {
	return &ToolHandle{tools: make(map[string]Tool)}
}

func (s *ToolHandle) Register(tool Tool) {
	if _, exists := s.tools[tool.Schema.Name]; !exists {
		s.order = append(s.order, tool.Schema.Name)
	}
	s.tools[tool.Schema.Name] = tool
}

// 返回所有工具的schema
func (s *ToolHandle) Schemas() []ToolSchema {
	schemas := make([]ToolSchema, 0, len(s.order))
	for _, name := range s.order {
		schemas = append(schemas, s.tools[name].Schema)
	}
	return schemas
}

func (s *ToolHandle) Run(ctx context.Context, name, arguments string) (string, error) {
	tool, exist := s.tools[name]
	if !exist {
		return "", fmt.Errorf("llm: unknown tool %q", name)
	}
	return tool.Run(ctx, arguments)
}
