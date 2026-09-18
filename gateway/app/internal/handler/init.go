// 本文件为手写扩展，注册 common/httpresponse 统一响应信封处理器。
// goctl 生成时不包含此文件；重新生成时不会覆盖。

package handler

import "go-video/common/httpresponse"

func init() {
	httpresponse.Install()
}
