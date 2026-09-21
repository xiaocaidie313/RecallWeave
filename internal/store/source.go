package store

import "time"

// Source 是一次导入：这段对话从哪来，原文是什么。
// 保留 RawText 是为了以后重新切分时还能回到原始内容。
type Source struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:191;not null" json:"name"`
	RawText   string    `gorm:"type:longtext" json:"raw_text"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message 是从一次导入里切出来的单条对话，Seq 用来还原顺序。
type Message struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SourceID  uint      `gorm:"index;not null" json:"source_id"`
	Seq       int       `gorm:"not null" json:"seq"`
	Role      string    `gorm:"size:32;not null" json:"role"`
	Content   string    `gorm:"type:text;not null" json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
