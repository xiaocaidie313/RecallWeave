#Requires -Version 5.1
[CmdletBinding()]
param(
    [uri]$BaseUrl = 'http://127.0.0.1:8008',
    [uint64]$ConversationId,
    [uint64]$SessionId,
    [int]$SeqStart,
    [int]$SeqEnd,
    [uint64]$ResumeFrom,
    [string]$Question
)

$ErrorActionPreference = 'Stop'
if (-not $BaseUrl.IsAbsoluteUri -or $BaseUrl.Scheme -notin 'http', 'https') {
    throw 'BaseUrl 必须是完整的 HTTP 或 HTTPS 服务地址。'
}
$base = $BaseUrl.AbsoluteUri.TrimEnd('/')
foreach ($name in 'ConversationId', 'SessionId', 'SeqStart', 'SeqEnd', 'ResumeFrom') {
    if ($PSBoundParameters.ContainsKey($name) -and $PSBoundParameters[$name] -lt 1) {
        throw "$name 必须是正整数。"
    }
}
$source = $null
if ($SessionId -or $SeqStart -or $SeqEnd) {
    if (-not $SessionId -or -not $SeqStart) { throw '选择旧笔记需要同时指定 SessionId 和 SeqStart。' }
    if (-not $SeqEnd) { $SeqEnd = $SeqStart }
    if ($SeqEnd -lt $SeqStart) { throw 'SeqEnd 必须大于或等于 SeqStart。' }
    if ($SeqEnd - $SeqStart -ge 50) { throw '每次最多选择 50 条原始消息。' }
    $source = @{ session_id = $SessionId; seq_start = $SeqStart; seq_end = $SeqEnd }
}

function Send-Json([string]$Path, [hashtable]$Payload) {
    $body = $Payload | ConvertTo-Json -Depth 6 -Compress
    Invoke-RestMethod -Uri "$base$Path" -Method Post -ContentType 'application/json; charset=utf-8' -Body ([Text.Encoding]::UTF8.GetBytes($body))
}

if (-not $ConversationId) {
    $conversation = Send-Json '/api/conversations/create' @{ title = '接着想' }
    $ConversationId = $conversation.id
}
$checkpointPath = "/api/conversations/$ConversationId/thinking/checkpoint"
$saved = (Invoke-RestMethod -Uri "$base$checkpointPath").checkpoint
Write-Host "思考对话 $ConversationId，下次使用 -ConversationId $ConversationId 回到这里。"
if ($saved) { Write-Host "上次留下的话 [$($saved.direction)] $($saved.text)" }
Write-Host '输入想法即可对话。/continue 从原文或停留点继续。/save 方向、/reject 理由、/reframe 新问题会替换当前停留点。/exit 退出。'

$startNow = $source -or $ResumeFrom -or -not [string]::IsNullOrWhiteSpace($Question)
$reader = $null
if ([Console]::IsInputRedirected) {
    $reader = [IO.StreamReader]::new([Console]::OpenStandardInput(), [Text.Encoding]::UTF8)
}
while ($true) {
    if ($startNow) {
        $turnText = $Question
        $startNow = $false
    } else {
        if ($reader) {
            $line = $reader.ReadLine()
        } else {
            $line = Read-Host '你'
        }
        if ($null -eq $line -or $line -eq '/exit') { break }
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        if ($line -match '^/(save|reject|reframe)(?:\s+(.+))?$') {
            $direction = @{ save = 'explore'; reject = 'reject'; reframe = 'reframe' }[$Matches[1]]
            $note = $Matches[2]
            if ([string]::IsNullOrWhiteSpace($note)) {
                Write-Warning '请在命令后写下你自己的想法。'
                continue
            }
            try {
                $saved = (Send-Json $checkpointPath @{ text = $note; direction = $direction }).checkpoint
                Write-Host "已保存 [$($saved.direction)] $($saved.text)"
            } catch { Write-Warning $_ }
            continue
        }
        if ($line -eq '/continue') {
            $turnText = ''
        } elseif ($line.StartsWith('/')) {
            Write-Warning '可用命令为 /continue、/save、/reject、/reframe、/exit。'
            continue
        } else {
            $turnText = $line
        }
    }
    $request = @{ mode = 'thinking'; question = [string]$turnText }
    if ($source) { $request.source = $source }
    if ($ResumeFrom) { $request.resume_from = $ResumeFrom }
    try {
        $answer = Send-Json "/api/chat/$ConversationId" $request
        $source = $null
        $ResumeFrom = 0
        Write-Host $answer.answer
        $anchor = $answer.thinking.source
        if ($anchor) { Write-Host "选中的原文 $($anchor.session_id) / 消息 $($anchor.seq_start)–$($anchor.seq_end)" }
        foreach ($citation in $answer.citations) {
            Write-Host "原文 $($citation.session_id) / 消息 $($citation.seq_start)–$($citation.seq_end)  $($citation.title)"
        }
        Write-Host "Run $($answer.run_id)"
    } catch { Write-Warning $_ }
}
