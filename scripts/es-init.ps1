# 校验/初始化本地 OpenSearch：索引结构、分词器与样例文档。
# 职责边界：本脚本只做「验证 + 灌开发索引」。生产写入索引与别名切换属于
# search-indexer 的重建流程（gateway/admin 的 POST /admin/search/rebuild），
# 脚本绝不创建或删除别名。
# 索引结构唯一来源是 services/search-indexer/internal/esclient，脚本每次现场
# 调 cmd/esmapping 导出，不缓存到仓库（详见 deploy/opensearch/README.md）。
param(
    [string]$Endpoint = 'http://127.0.0.1:9200',
    # 必须显式给物理索引名；名字已是别名时脚本直接拒绝，避免写坏查询目标。
    [string]$Index = 'go_video_content_dev_v1',
    [string]$SchemaVersion = 'v1',
    [ValidateSet('cjk', 'ik', 'smartcn', 'standard')]
    [string]$Analyzer = 'cjk',
    # 词典在集群容器内的绝对路径；传 '' 表示该词典不启用（与 etc 示例保持一致）。
    [string]$StopwordsPath = '/usr/share/opensearch/config/analysis/go_video_stopwords.txt',
    [string]$SynonymsPath = '/usr/share/opensearch/config/analysis/go_video_synonyms.txt',
    [string]$Username = '',
    [string]$Password = '',
    # 分词验证文本与冒烟查询词。
    [string]$AnalyzeText = 'OpenSearch 中文分词与索引重建实践',
    [string]$QueryTerm = '分词',
    [switch]$SkipCreate,
    [switch]$SkipDocs,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

function Invoke-EsApi {
    param(
        [Parameter(Mandatory)][string]$Method,
        [Parameter(Mandatory)][string]$Path,
        [string]$BodyJson = '',
        # $Accept 传空串表示不声明 Accept 头（_refresh 等接口返回 text/plain）。
        [AllowEmptyString()][string]$Accept = 'application/json'
    )
    $headers = @{}
    if ($Username -ne '' -or $Password -ne '') {
        $headers['Authorization'] = 'Basic ' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes("${Username}:${Password}"))
    }
    $params = @{
        Uri             = $Endpoint.TrimEnd('/') + $Path
        Method          = $Method
        Headers         = $headers
        ContentType     = 'application/json; charset=utf-8'
        UseBasicParsing = $true
        TimeoutSec      = 60
        ErrorAction     = 'Stop'
    }
    if ($Accept) { $params['Headers']['Accept'] = $Accept }
    if ($BodyJson) {
        # 中文词典/文档必须按 UTF-8 字节发出：PowerShell 5.1 传字符串会按本地代码页编码。
        $params['Body'] = [Text.Encoding]::UTF8.GetBytes($BodyJson)
    }
    $resp = Invoke-WebRequest @params
    if ($resp.Content) { return $resp.Content }
    return ''
}

function Invoke-EsApiTolerant {
    param([string]$Method, [string]$Path, [string]$BodyJson = '', [string]$Accept = 'application/json')
    try { return [pscustomobject]@{ Ok = $true; Body = (Invoke-EsApi -Method $Method -Path $Path -BodyJson $BodyJson -Accept $Accept) } }
    catch { return [pscustomobject]@{ Ok = $false; Body = $_.Exception.Message } }
}

function Write-Step([string]$Message) { Write-Host "==> $Message" -ForegroundColor Cyan }

# ---------- 1. 集群健康 ----------
Write-Step "等待集群就绪 $Endpoint"
$ready = Invoke-EsApiTolerant -Method Get -Path '/_cluster/health?wait_for_status=yellow&timeout=45s'
if (-not $ready.Ok) {
    throw "集群不可用（先执行 docker compose -f deploy/docker-compose/docker-compose.yml up -d opensearch）：$($ready.Body)"
}
$health = $ready.Body | ConvertFrom-Json
Write-Host ("集群 {0} status={1} nodes={2}" -f $health.cluster_name, $health.status, $health.number_of_nodes)

# ---------- 2. 插件实测（不要凭镜像标签假设分词插件已装） ----------
Write-Step '检查已安装的分析插件'
$plugins = @()
$pluginProbe = Invoke-EsApiTolerant -Method Get -Path '/_cat/plugins?format=json'
if ($pluginProbe.Ok -and $pluginProbe.Body) {
    $plugins = @($pluginProbe.Body | ConvertFrom-Json | ForEach-Object { $_.component })
}
if ($plugins.Count -eq 0) { Write-Host '（_cat/plugins 无输出，按未安装插件处理）' }
else { Write-Host ($plugins -join ', ') }
$requiredPlugin = @{ ik = 'analysis-ik'; smartcn = 'analysis-smartcn' }[$Analyzer]
if ($requiredPlugin -and -not @($plugins | Where-Object { $_ -like "*$requiredPlugin*" }).Count) {
    throw "-Analyzer $Analyzer 需要插件 $requiredPlugin，但集群未安装。改用 -Analyzer cjk，或按 deploy/opensearch/README.md 构建带插件的镜像。"
}

# ---------- 3. 现场导出索引结构（唯一来源：Go） ----------
Write-Step "导出索引结构 schema=$SchemaVersion analyzer=$Analyzer"
$indexBodyFile = Join-Path ([IO.Path]::GetTempPath()) ('go-video-es-' + [Guid]::NewGuid().ToString('N') + '.json')
& go run ./services/search-indexer/cmd/esmapping "-schema-version=$SchemaVersion" "-analyzer=$Analyzer" "-stopwords-path=$StopwordsPath" "-synonyms-path=$SynonymsPath" "-out=$indexBodyFile"
if ($LASTEXITCODE -ne 0) { throw "esmapping 导出索引结构失败（exit=$LASTEXITCODE）" }
if (-not (Test-Path $indexBodyFile)) { throw 'esmapping 未生成索引结构文件' }
$indexBody = [IO.File]::ReadAllText($indexBodyFile, [Text.Encoding]::UTF8)
Remove-Item $indexBodyFile -Force

# ---------- 4. 建物理索引（幂等；别名一律拒绝） ----------
$aliasProbe = Invoke-EsApiTolerant -Method Get -Path "/_cat/aliases/$Index"
if ($aliasProbe.Ok -and $aliasProbe.Body) {
    if (-not $Force) { throw "$Index 已经是别名（$($aliasProbe.Body.Trim())）。脚本只写物理索引，换一个名字；确认可控后再加 -Force。" }
    Write-Host '! 目标是别名，已按 -Force 继续（会把文档写进别名当前指向的物理索引）' -ForegroundColor Yellow
}
if ($SkipCreate) {
    Write-Step '-SkipCreate：跳过建索引'
} elseif ((Invoke-EsApiTolerant -Method Get -Path "/$Index").Ok) {
    Write-Step "索引 $Index 已存在，跳过创建（结构以现状为准，下一步校验 _meta）"
} else {
    Write-Step "创建物理索引 $Index"
    $created = Invoke-EsApiTolerant -Method Put -Path "/$Index" -BodyJson $indexBody
    if (-not $created.Ok) { throw "创建索引失败：$($created.Body)" }
}

# ---------- 5. _meta 与本次配置是否同源 ----------
Write-Step '校验索引 _meta（结构版本与分词族）'
$mappings = (Invoke-EsApi -Method Get -Path "/$Index/_mapping") | ConvertFrom-Json
$meta = $mappings.$Index.mappings._meta
if (-not $meta) { throw "索引 $Index 没有 mappings._meta，不是本仓库建出来的索引（检查 -Index 是否写错）" }
if ($meta.schema_version -ne $SchemaVersion -and -not $Force) {
    throw "_meta.schema_version=$($meta.schema_version) 与 -SchemaVersion $SchemaVersion 不一致：结构变更必须递增版本并走重建"
}
if ($meta.analyzer -ne $Analyzer -and -not $Force) {
    throw "_meta.analyzer=$($meta.analyzer) 与 -Analyzer $Analyzer 不一致：分词族变更等于结构变更，请递增 SchemaVersion 后重建"
}
Write-Host ("_meta OK: schema={0} analyzer={1}" -f $meta.schema_version, $meta.analyzer)

# ---------- 6. 分词结果验证（写入/查询两侧都要看） ----------
Write-Step '_analyze 分词验证'
foreach ($analyzerName in @('go_video_title', 'go_video_search')) {
    $body = @{ analyzer = $analyzerName; text = $AnalyzeText } | ConvertTo-Json -Compress
    $tokens = @((Invoke-EsApi -Method Post -Path "/$Index/_analyze" -BodyJson $body | ConvertFrom-Json).tokens)
    Write-Host ("{0,-17} -> {1}" -f $analyzerName, (($tokens | ForEach-Object { $_.token }) -join ' | '))
    if ($tokens.Count -eq 0) { throw "$analyzerName 没有产出任何 token，分词配置无效" }
}
if ($Analyzer -eq 'cjk') {
    # cjk 策略的中文必须被切成二元组，否则说明 bigram filter 没挂上（等价于没做中文分词）。
    $probe = @{ analyzer = 'go_video_search'; text = '中文分词' } | ConvertTo-Json -Compress
    $bigrams = @((Invoke-EsApi -Method Post -Path "/$Index/_analyze" -BodyJson $probe | ConvertFrom-Json).tokens | ForEach-Object { $_.token })
    foreach ($want in @('中文', '文分', '分词')) {
        if (-not ($bigrams -contains $want)) { throw "cjk 策略未产出预期二元组 $want，实际：$($bigrams -join ' | ')" }
    }
}

# ---------- 7. 灌样例文档（_id = <content_type>_<content_id>，可重复执行） ----------
if (-not $SkipDocs) {
    Write-Step "写入样例文档到 $Index"
    $samplePath = Join-Path $repoRoot 'deploy\opensearch\samples\content.sample.ndjson'
    if (-not (Test-Path $samplePath)) { throw "样例文档缺失：$samplePath" }
    $lines = @(Get-Content -Path $samplePath -Encoding UTF8 | Where-Object { $_ -and ($_ -notmatch '^\s*#') })
    if ($lines.Count -eq 0) { throw '样例文档为空' }
    $sb = New-Object Text.StringBuilder
    foreach ($line in $lines) {
        $doc = $line | ConvertFrom-Json
        # _id 规则与服务侧一致（esclient.DocID），保证脚本灌的数据与进程写的数据同键。
        $action = @{ index = @{ _index = $Index; _id = "$($doc.content_type)_$($doc.content_id)" } } | ConvertTo-Json -Compress
        [void]$sb.AppendLine($action)
        [void]$sb.AppendLine($line)
    }
    $bulk = $sb.ToString()
    if (-not $bulk.EndsWith("`n")) { $bulk += "`n" }
    $bulkResp = Invoke-EsApi -Method Post -Path '/_bulk' -BodyJson $bulk
    if ($bulkResp -match '"errors"\s*:\s*true') { throw "_bulk 存在失败条目：$bulkResp" }
    $null = Invoke-EsApi -Method Post -Path "/$Index/_refresh" -Accept ''
    $count = [long]((Invoke-EsApi -Method Get -Path "/$Index/_count") | ConvertFrom-Json).count
    Write-Host "文档数 $count（样例 $($lines.Count) 条）"
    if ($count -lt $lines.Count) { throw "文档数少于样例条数，检查是否有 _id 互相覆盖" }

    # ---------- 8. 冒烟查询：中文命中 + 不可检索状态不出现 ----------
    Write-Step "冒烟查询 '$QueryTerm'"
    $query = @{
        query  = @{
            bool = @{
                must   = @(@{ multi_match = @{ query = $QueryTerm; fields = @('title^3', 'description', 'author_name^2', 'tags'); type = 'best_fields' } })
                filter = @(@{ term = @{ state = 2 } })
            }
        }
        size   = 5
        _source = @('content_id', 'content_type', 'title', 'state')
    } | ConvertTo-Json -Depth 8 -Compress
    $result = Invoke-EsApi -Method Post -Path "/$Index/_search" -BodyJson $query | ConvertFrom-Json
    $hits = @($result.hits.hits)
    Write-Host ("命中 {0} 条（total={1}）" -f $hits.Count, $result.hits.total.value)
    foreach ($h in $hits) { Write-Host ("  [state={0}/{1}] {2}" -f $h._source.state, $h._id, $h._source.title) }
    if ($hits.Count -eq 0) { throw "中文查询零命中：对照第 6 步的 _analyze 输出检查分词或样例数据" }
    if (@($hits | Where-Object { $_._source.state -ne 2 }).Count -gt 0) { throw 'state 过滤失效：不可检索状态被命中' }
}

# ---------- 9. 别名现状（只读展示） ----------
Write-Step '别名现状'
$aliases = Invoke-EsApiTolerant -Method Get -Path '/_cat/aliases?format=json'
if ($aliases.Ok -and $aliases.Body) {
    foreach ($a in ($aliases.Body | ConvertFrom-Json)) { Write-Host ("  {0} -> {1}" -f $a.alias, $a.index) }
} else {
    Write-Host '（当前没有任何别名；search-query 读的是 OpenSearch.IndexPrefix 对应别名，由 search-indexer 重建流程创建）'
}
Write-Host 'es-init 全部检查通过' -ForegroundColor Green
