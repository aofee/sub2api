package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// fork 的在线更新必须只看本仓库：rebase 上游时若被改回 Wei-Shaw/sub2api，
// 后台一键更新会用未打补丁的上游二进制覆盖容器内的程序。
func TestUpdateServiceUsesForkRepository(t *testing.T) {
	require.Equal(t, "aofee/sub2api", githubRepo)
}
