package store

// ---------- 来源分类 ----------

// SourceTag 是一段会话的来源平台。存字符串而不是数字 ID，
// 查库时能直接看懂，也不用额外维护一份映射。
type SourceTag string

const (
	SourceWX          SourceTag = "wx"
	SourceQQ          SourceTag = "qq"
	SourceWeibo       SourceTag = "weibo"
	SourceTwitter     SourceTag = "twitter"
	SourceFacebook    SourceTag = "facebook"
	SourceInstagram   SourceTag = "instagram"
	SourceYoutube     SourceTag = "youtube"
	SourceTiktok      SourceTag = "tiktok"
	SourceCursorIDE   SourceTag = "cursor_ide"
	SourceCursorAgent SourceTag = "cursor_agent"
	SourceCodex       SourceTag = "codex"
	SourceChatGPT     SourceTag = "chatgpt"
	SourceChat        SourceTag = "chat"
)

var sourceTags = map[SourceTag]struct{}{
	SourceWX:          {},
	SourceQQ:          {},
	SourceWeibo:       {},
	SourceTwitter:     {},
	SourceFacebook:    {},
	SourceInstagram:   {},
	SourceYoutube:     {},
	SourceTiktok:      {},
	SourceCursorIDE:   {},
	SourceCursorAgent: {},
	SourceCodex:       {},
	SourceChatGPT:     {},
	SourceChat:        {},
}

func IsValidSourceTag(tag SourceTag) bool {
	_, ok := sourceTags[tag]
	return ok
}

// ---------- 主题标签 ----------

// MemoryTag 是提炼时允许使用的主题标签。用受控词表是因为放任模型自由发挥时，
// 同一件事会拿到 service 和 after-sales 这种同义标签，按标签检索就会漏。
type MemoryTag string

const (
	TagWork     MemoryTag = "work"
	TagStudy    MemoryTag = "study"
	TagTech     MemoryTag = "tech"
	TagProduct  MemoryTag = "product"
	TagDecision MemoryTag = "decision"
	TagPeople   MemoryTag = "people"
	TagLife     MemoryTag = "life"
	TagOther    MemoryTag = "other"
)

// MemoryTags 会写进 prompt 交给模型选择，顺序固定方便排查。
var MemoryTags = []MemoryTag{
	TagWork,
	TagStudy,
	TagTech,
	TagProduct,
	TagDecision,
	TagPeople,
	TagLife,
	TagOther,
}

// NormalizeMemoryTag 把模型返回的标签收敛到词表内，认不出来的一律记 other。
func NormalizeMemoryTag(raw string) MemoryTag {
	for _, tag := range MemoryTags {
		if string(tag) == raw {
			return tag
		}
	}
	return TagOther
}

// ---------- 记忆内容类型 ----------

// ContentType 说明这条记忆是哪一类内容。检索时用来区分「我当时的偏好」
// 和「当时下的结论」，这两种在回答里的权重不一样。
type ContentType string

const (
	ContentFact       ContentType = "fact"       // 客观事实
	ContentPreference ContentType = "preference" // 个人偏好
	ContentDecision   ContentType = "decision"   // 做出的决定
	ContentTask       ContentType = "task"       // 待办或计划
	ContentQuestion   ContentType = "question"   // 提出的疑问
	ContentOther      ContentType = "other"
)

var ContentTypes = []ContentType{
	ContentFact,
	ContentPreference,
	ContentDecision,
	ContentTask,
	ContentQuestion,
	ContentOther,
}

func NormalizeContentType(raw string) ContentType {
	for _, contentType := range ContentTypes {
		if string(contentType) == raw {
			return contentType
		}
	}
	return ContentOther
}

// ---------- 情绪分类 ----------

// Emotion 是这段对话的情绪倾向。先只分三档，细分到喜怒哀乐意义不大，
// 而且模型在细粒度上一致性很差。
type Emotion string

const (
	EmotionPositive   Emotion = "positive"
	EmotionNeutral    Emotion = "neutral"
	EmotionNegative   Emotion = "negative"
	EmotionPeaceful   Emotion = "peaceful"
	EmotionAngry      Emotion = "angry"
	EmotionSad        Emotion = "sad"
	EmotionHappy      Emotion = "happy"
	EmotionSurprised  Emotion = "surprised"
	EmotionCurious    Emotion = "curious"
	EmotionConfused   Emotion = "confused"
	EmotionBored      Emotion = "bored"
	EmotionStressed   Emotion = "stressed"
	EmotionAnxious    Emotion = "anxious"
	EmotionDepressed  Emotion = "depressed"
	EmotionExited     Emotion = "exited"
	EmotionRelaxed    Emotion = "relaxed"
	EmotionCalm       Emotion = "calm"
	EmotionFocused    Emotion = "focused"
	EmotionDistracted Emotion = "distracted"
)

var Emotions = []Emotion{
	EmotionPositive,
	EmotionNeutral,
	EmotionNegative,
}

func NormalizeEmotion(raw string) Emotion {
	for _, emotion := range Emotions {
		if string(emotion) == raw {
			return emotion
		}
	}
	return EmotionNeutral
}

// ---------- 记忆优先级 ----------

// Priority 决定检索时的排序权重，数字越大越优先。
type Priority int

const (
	PriorityLow    Priority = 1
	PriorityNormal Priority = 2
	PriorityHigh   Priority = 3
)

func IsValidPriority(priority Priority) bool {
	return priority >= PriorityLow && priority <= PriorityHigh
}
