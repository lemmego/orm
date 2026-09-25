// Package genfixture holds models used to exercise the ormgen generator.
// orm_fields.go in this directory is generated from these declarations and is
// checked for drift by the generator's tests.
package genfixture

import (
	"database/sql"
	"time"
)

// Timestamps is embedded, so its columns must be inlined into each model that
// carries it, exactly as the runtime inlines them.
type Timestamps struct {
	CreatedAt time.Time
	UpdatedAt time.Time
}

type User struct {
	Timestamps
	ID        int    `orm:"primaryKey;autoIncrement"`
	Email     string `orm:"column:email_address"`
	Age       int
	Nickname  *string
	Bio       sql.NullString
	Active    bool
	DeletedAt *time.Time

	Secret   string `orm:"-"`
	internal string

	Posts   []Post   `orm:"hasMany:UserID"`
	Profile *Profile `orm:"hasOne:UserID"`
	Tags    []Tag    `orm:"many2many:user_tags"`
}

type Post struct {
	ID     int `orm:"primaryKey;autoIncrement"`
	UserID int
	Title  string
	Author *User `orm:"belongsTo:UserID"`
}

type Profile struct {
	ID     int `orm:"primaryKey;autoIncrement"`
	UserID int
	Avatar string
}

type Tag struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

// Category exercises pluralisation the old naive rule got wrong.
type Category struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

// Account overrides its table name, which the generator reads off the method.
type Account struct {
	ID   int `orm:"primaryKey;autoIncrement"`
	Name string
}

func (Account) TableName() string { return "billing_accounts" }

// LegacyUser uses db tags, as the CLI's model generator emits today.
type LegacyUser struct {
	ID    uint64 `db:"id,omitempty"`
	Email string `db:"email"`
	Skip  string `db:"-"`
}

// NotAModel carries no mapping tags and must be skipped by default.
type NotAModel struct {
	Something string
}
