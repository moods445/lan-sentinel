// Package sql 嵌入迁移脚本，保证单二进制即可初始化数据库。
package sql

import _ "embed"

//go:embed 001_init.sql
var Init string
