package logic

// 系统站内信投递的参数守卫与默认值补齐（SendSystemMessage）。
//
// 幂等键必填且被 trim，服务端不代造——否则调用方重试会重复投递（AGENTS.md §5）。
// 主链路与故障传播见 sendsystemmessagelogic_test.go。

import (
	"context"
	"strings"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

func TestSendSystemMessageGuards(t *testing.T) {
	longKey := strings.Repeat("k", 129)
	longBody := strings.Repeat("x", 65) // 65 字节 > 测试配置里的 MaxContentBytes=64
	cases := []struct {
		name string
		req  *rpc.SendSystemMessageReq
		want error
		msg  string
	}{
		{
			name: "缺幂等键",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{midAlice}, Title: "标题"},
			want: model.ErrInvalidIdempotencyKey,
		},
		{
			name: "幂等键只有空白",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{midAlice}, Title: "标题", IdempotencyKey: "   \t "},
			want: model.ErrInvalidIdempotencyKey,
		},
		{
			name: "幂等键超过 128",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{midAlice}, Title: "标题", IdempotencyKey: longKey},
			want: model.ErrInvalidIdempotencyKey,
		},
		{
			name: "无接收人",
			req:  &rpc.SendSystemMessageReq{Title: "标题", IdempotencyKey: "key-1"},
			want: model.ErrEmptyRecipients,
		},
		{
			name: "接收人全非法",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{0, -7}, Title: "标题", IdempotencyKey: "key-1"},
			want: model.ErrEmptyRecipients,
		},
		{
			name: "去重后接收人超上限",
			req: &rpc.SendSystemMessageReq{
				Mids: []int64{midAlice, midBob, midCarol, 91004}, Title: "标题", IdempotencyKey: "key-1",
			},
			want: model.ErrTooManyRecipients,
		},
		{
			name: "标题与正文都为空",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{midAlice}, IdempotencyKey: "key-1"},
			want: model.ErrEmptyContent,
		},
		{
			name: "正文只有空白",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{midAlice}, Content: "  \n ", IdempotencyKey: "key-1"},
			want: model.ErrEmptyContent,
		},
		{
			name: "正文超过字节上限",
			req:  &rpc.SendSystemMessageReq{Mids: []int64{midAlice}, Content: longBody, IdempotencyKey: "key-1"},
			msg:  "exceeds 64 bytes",
		},
		{
			name: "分类越界",
			req: &rpc.SendSystemMessageReq{
				Mids: []int64{midAlice}, Title: "标题", IdempotencyKey: "key-1", Category: rpc.Category(9),
			},
			want: model.ErrInvalidCategory,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(tc.req)

			if reply != nil {
				t.Fatalf("%s：拒绝后仍返回响应体 %+v", tc.name, reply)
			}
			switch {
			case tc.want != nil:
				wantErrIs(t, tc.name, err, tc.want)
			case tc.msg != "":
				wantErrContains(t, tc.name, err, tc.msg)
			default:
				t.Fatalf("%s：用例没写期望错误", tc.name)
			}
			// 守卫必须发生在触库之前：一条 SQL、一次缓存操作都不允许。
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

func TestSendSystemMessageNilRequestIsRejected(t *testing.T) {
	e := newEnv(t)
	before := e.st.log.snapshot()

	reply, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(nil)

	if reply != nil {
		t.Fatalf("nil 请求仍返回响应体 %+v", reply)
	}
	wantErrIs(t, "nil 请求", err, model.ErrEmptyRecipients)
	wantNoCall(t, "nil 请求", e.st, before)
}

// 未指定的分类与载体类型按「系统通知 + 纯文本」补齐，extra 空串补成 {}，
// 这样下游展示不需要再判空。
func TestSendSystemMessageFillsDefaults(t *testing.T) {
	e := newEnv(t)
	st := e.st

	reply, err := NewSendSystemMessageLogic(context.Background(), e.svcCtx).SendSystemMessage(
		&rpc.SendSystemMessageReq{
			Mids: []int64{midAlice}, Content: "只有正文，标题留空",
			IdempotencyKey: "defaults-1",
		})
	wantNoErr(t, "只给正文的投递", err)

	stored := st.getMessage(t, reply.MsgId)
	if stored == nil {
		t.Fatalf("消息主体没落库，reply=%+v", reply)
	}
	wantEQ(t, "默认值", "Category", stored.Category, model.CategorySystem)
	wantEQ(t, "默认值", "MsgType", stored.MsgType, model.MsgTypeText)
	wantEQ(t, "默认值", "Extra", stored.Extra, "{}")
	wantEQ(t, "默认值", "Title", stored.Title, "")
	wantEQ(t, "默认值", "Content", stored.Content, "只有正文，标题留空")
}
