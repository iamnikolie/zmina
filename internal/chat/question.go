package chat

// Question is a question a role put to the owner on its screen (Claude Code's AskUserQuestion) and has not had
// answered yet: one or more items, each with its options. ID is the tool call's, so an answer can be matched to it.
type Question struct {
	ID    string         `json:"id"`
	Items []QuestionItem `json:"items"`
}

// QuestionItem is one question of a Question.
type QuestionItem struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []QuestionOption `json:"options"`
}

// QuestionOption is one choice; a label ending in «(Recommended)» is the role's recommendation.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
