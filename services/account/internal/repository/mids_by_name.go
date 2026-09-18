package repository

import (
	"context"

	"go-video/services/account/model"
)

// MidsByName 按用户名凭证查询 mid 列表。
// 凭证类型为 username（credential_type=1），返回 name→mid 的映射。
// 命中数限制在 100 个以内，避免无界查询。
func (r *Repository) MidsByName(ctx context.Context, names []string) (map[string]int64, error) {
	if len(names) == 0 {
		return map[string]int64{}, nil
	}
	if len(names) > 100 {
		names = names[:100]
	}
	return r.credentialModel.FindMidsByIdentifiers(ctx, model.CredentialTypeUsername, names)
}
