// esmapping 把 search-indexer 的索引结构（settings + mappings，含分词配置）导出为 JSON，
// 供 deploy/opensearch 下的脚本与人工排障使用。
//
// 为什么要这个入口：索引 mapping 的唯一来源是 internal/esclient.IndexBody，
// 如果在 shell 里再抄一份 mapping/JSON 文件，两边一定会漂移（AGENTS.md §4 生成纪律同理）。
// 脚本一律调用本命令取结构，再决定发给集群什么。
//
// 用法（仓库根目录）：
//
//	go run ./services/search-indexer/cmd/esmapping -schema-version v1 -analyzer cjk
//	go run ./services/search-indexer/cmd/esmapping -pretty -out deploy/opensearch/index.dev.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go-video/services/search-indexer/internal/esclient"
)

func main() {
	var (
		schemaVersion = flag.String("schema-version", esclient.DefaultSchemaVersion, "索引结构版本，如 v1")
		kind          = flag.String("analyzer", string(esclient.DefaultAnalyzer().Kind), "分词插件族：cjk|ik|smartcn|standard")
		stopwordsPath = flag.String("stopwords-path", "", "停用词文件在集群容器内的绝对路径，留空表示不启用")
		synonymsPath  = flag.String("synonyms-path", "", "同义词文件在集群容器内的绝对路径，留空表示不启用")
		pretty        = flag.Bool("pretty", false, "缩进输出，便于人工查看与 diff")
		out           = flag.String("out", "", "写入文件路径，留空输出到 stdout")
	)
	flag.Parse()

	body, err := esclient.IndexBody(*schemaVersion, esclient.Analyzer{
		Kind:          esclient.AnalyzerKind(*kind),
		StopwordsPath: *stopwordsPath,
		SynonymsPath:  *synonymsPath,
	})
	if err != nil {
		fail(err)
	}
	if *pretty {
		var v interface{}
		if err := json.Unmarshal(body, &v); err != nil {
			fail(err)
		}
		buf, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fail(err)
		}
		body = append(buf, '\n')
	}

	if *out == "" {
		if _, err := os.Stdout.Write(body); err != nil {
			fail(err)
		}
		return
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fail(err)
	}
	fmt.Fprintln(os.Stderr, "esmapping: wrote "+*out+" (schema_version="+*schemaVersion+" analyzer="+*kind+")")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "esmapping:", err)
	os.Exit(1)
}
