package model

import "strings"

var TaskLabels = []string{"feature", "bugfix", "refactor", "docs", "chore"}

func ValidTaskLabel(label string) bool {
	for _, candidate := range TaskLabels {
		if label == candidate {
			return true
		}
	}
	return false
}

func InferTaskLabel(task Task) string {
	text := strings.ToLower(strings.Join([]string{
		task.Title,
		task.Description,
		task.Instruction,
		task.AcceptanceCriteria,
	}, "\n"))

	rules := []struct {
		label string
		terms []string
	}{
		{"bugfix", []string{"bug", "fix", "broken", "error", "failure", "regression", "不具合", "バグ", "修正", "直す", "動かない", "エラー"}},
		{"docs", []string{"docs", "documentation", "readme", "guide", "manual", "ドキュメント", "文書", "手順書", "説明を追加"}},
		{"refactor", []string{"refactor", "cleanup", "restructure", "simplify", "rename", "リファクタ", "整理", "構造変更", "共通化"}},
		{"feature", []string{"feature", "add", "implement", "support", "enable", "new ", "機能", "追加", "実装", "対応", "作成", "できるよう"}},
		{"chore", []string{"chore", "dependency", "dependencies", "upgrade", "bump", "ci", "build", "release", "設定", "依存", "更新", "アップグレード"}},
	}
	for _, rule := range rules {
		for _, term := range rule.terms {
			if strings.Contains(text, term) {
				return rule.label
			}
		}
	}
	return "chore"
}
