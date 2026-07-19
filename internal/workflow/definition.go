package workflow

import (
	"encoding/json"
	"time"
)

type NodeKind string

const (
	NodeSequence     NodeKind = "sequence"
	NodeParallel     NodeKind = "parallel"
	NodeLoop         NodeKind = "loop"
	NodeJoin         NodeKind = "join"
	NodeAgentTask    NodeKind = "agent_task"
	NodeApproval     NodeKind = "approval"
	NodeWaitEvent    NodeKind = "wait_event"
	NodeSubworkflow  NodeKind = "subworkflow"
	NodeCondition    NodeKind = "condition"
	NodeErrorHandler NodeKind = "error_handler"
)

type FailurePolicy string

const (
	FailurePolicyFailFast   FailurePolicy = "fail_fast"
	FailurePolicyCollectAll FailurePolicy = "collect_all"
)

type Status string

const (
	StatusCreated         Status = "created"
	StatusRunning         Status = "running"
	StatusCompleted       Status = "completed"
	StatusWaitingApproval Status = "waiting_approval"
	StatusWaitingEvent    Status = "waiting_event"
	StatusFailed          Status = "failed"
	StatusCompensated     Status = "compensated"
)

type Definition struct {
	ID        string            `json:"id"`
	CompanyID string            `json:"company_id,omitempty"`
	Variables map[string]string `json:"variables,omitempty"`
	Root      Node              `json:"root"`
}

type Node struct {
	ID       string   `json:"id"`
	Kind     NodeKind `json:"kind"`
	Children []Node   `json:"children,omitempty"`
	// Task carries no omitempty: encoding/json ignores it on struct types, so
	// every node marshals a "task" object even when Kind is not NodeTask. The
	// zero TaskSpec is the "no task" signal (executeNode checks Task.ID == "").
	// Making the key genuinely optional requires *TaskSpec, which is both a
	// wire-contract change and a nil-deref risk on the template-substitution
	// path that assigns to node.Task fields.
	Task                 TaskSpec      `json:"task"`
	Subject              string        `json:"subject,omitempty"`
	Reason               string        `json:"reason,omitempty"`
	EventType            string        `json:"event_type,omitempty"`
	EventTaskID          string        `json:"event_task_id,omitempty"`
	EventMessageContains string        `json:"event_message_contains,omitempty"`
	Subworkflow          *Definition   `json:"subworkflow,omitempty"`
	ConditionKey         string        `json:"condition_key,omitempty"`
	ConditionValue       string        `json:"condition_value,omitempty"`
	Then                 *Node         `json:"then,omitempty"`
	Else                 *Node         `json:"else,omitempty"`
	Try                  *Node         `json:"try,omitempty"`
	Handler              *Node         `json:"handler,omitempty"`
	FailurePolicy        FailurePolicy `json:"failure_policy,omitempty"`
	LoopMaxIterations    int           `json:"loop_max_iterations,omitempty"`
	LoopBody             *Node         `json:"loop_body,omitempty"`
	Quorum               int           `json:"quorum,omitempty"`
	Timeout              time.Duration `json:"timeout,omitempty"`
}

type TaskSpec struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Input     string `json:"input"`
}

type Result struct {
	WorkflowID string       `json:"workflow_id"`
	Status     Status       `json:"status"`
	Nodes      []NodeResult `json:"nodes"`
}

type NodeResult struct {
	NodeID string `json:"node_id"`
	Status Status `json:"status"`
}

func (n *Node) UnmarshalJSON(data []byte) error {
	type nodeAlias Node
	aux := struct {
		*nodeAlias
		EventTypeLegacy            string        `json:"EventType"`
		EventTaskIDLegacy          string        `json:"EventTaskID"`
		EventMessageContainsLegacy string        `json:"EventMessageContains"`
		ConditionKeyLegacy         string        `json:"ConditionKey"`
		ConditionValueLegacy       string        `json:"ConditionValue"`
		FailurePolicyLegacy        FailurePolicy `json:"FailurePolicy"`
		LoopMaxIterationsLegacy    int           `json:"LoopMaxIterations"`
		LoopBodyLegacy             *Node         `json:"LoopBody"`
		QuorumLegacy               int           `json:"Quorum"`
		TimeoutLegacy              time.Duration `json:"Timeout"`
	}{nodeAlias: (*nodeAlias)(n)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if n.EventType == "" {
		n.EventType = aux.EventTypeLegacy
	}
	if n.EventTaskID == "" {
		n.EventTaskID = aux.EventTaskIDLegacy
	}
	if n.EventMessageContains == "" {
		n.EventMessageContains = aux.EventMessageContainsLegacy
	}
	if n.ConditionKey == "" {
		n.ConditionKey = aux.ConditionKeyLegacy
	}
	if n.ConditionValue == "" {
		n.ConditionValue = aux.ConditionValueLegacy
	}
	if n.FailurePolicy == "" {
		n.FailurePolicy = aux.FailurePolicyLegacy
	}
	if n.LoopMaxIterations == 0 {
		n.LoopMaxIterations = aux.LoopMaxIterationsLegacy
	}
	if n.LoopBody == nil {
		n.LoopBody = aux.LoopBodyLegacy
	}
	if n.Quorum == 0 {
		n.Quorum = aux.QuorumLegacy
	}
	if n.Timeout == 0 {
		n.Timeout = aux.TimeoutLegacy
	}
	return nil
}
