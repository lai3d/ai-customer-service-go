import { useState } from 'react'
import { Alert, Button, Card, Descriptions, Input, Select, Space, Table, Tag, Typography } from 'antd'
import { api } from '../api/client'
import type { ConversationSummary, Turn } from '../api/types'
import { Markdown } from '../components/Markdown'
import { useLoad, when } from './hooks'
import { usePaging } from './paging'

const OUTCOMES = ['completed', 'cancelled', 'failed', 'tool_limit', 'budget_exceeded',
  'retrieval_failed', 'memory_failed', 'in_flight']

export function ConversationsPage() {
  const [open, setOpen] = useState<string | null>(null)
  return open
    ? <Conversation id={open} onBack={() => setOpen(null)} />
    : <List onOpen={setOpen} />
}

function List({ onOpen }: { onOpen: (id: string) => void }) {
  const [filter, setFilter] = useState<{ outcome?: string; q?: string }>({})
  const [applied, setApplied] = useState<{ outcome?: string; q?: string }>({})
  const paging = usePaging()
  const { data, error, loading } = useLoad(
    () => api.conversations({ ...applied, ...paging.window }),
    [applied, paging.page, paging.pageSize])

  // A filter changes what is being counted, so page 7 of the old result is not page 7 of
  // the new one -- and asking the server for that offset returns an empty table that
  // looks like "no matches".
  const apply = (next: { outcome?: string; q?: string }) => {
    paging.reset()
    setApplied(next)
  }

  return (
    <>
      <Space style={{ marginBottom: 12 }} wrap>
        <Select
          style={{ width: 200 }}
          value={filter.outcome ?? ''}
          onChange={(v) => setFilter({ ...filter, outcome: v || undefined })}
          options={[{ value: '', label: 'any outcome' },
            ...OUTCOMES.map((o) => ({ value: o, label: o }))]}
        />
        <Input
          style={{ width: 260 }}
          allowClear
          placeholder="search the conversation id"
          value={filter.q ?? ''}
          onChange={(e) => setFilter({ ...filter, q: e.target.value || undefined })}
          onPressEnter={() => apply(filter)}
        />
        <Button onClick={() => apply(filter)}>Filter</Button>
      </Space>

      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}

      <Table<ConversationSummary>
        size="small"
        loading={loading}
        rowKey="conversationId"
        dataSource={data?.conversations ?? []}
        onRow={(row) => ({ onClick: () => onOpen(row.conversationId), style: { cursor: 'pointer' } })}
        scroll={{ x: 'max-content' }}
        pagination={paging.pagination(data?.total ?? 0, (n) => `${n} conversation(s)`)}
        columns={[
          { title: 'conversation', dataIndex: 'conversationId', className: 'mono' },
          { title: 'turns', dataIndex: 'turns', align: 'right' },
          {
            title: 'outcomes', dataIndex: 'outcomes',
            render: (o: string[]) => o.map((x) => <Tag key={x} color={tagColour(x)}>{x}</Tag>),
          },
          {
            title: 'tokens', align: 'right',
            render: (_, r) => (r.inputTokens + r.outputTokens).toLocaleString(),
          },
          { title: 'tickets', dataIndex: 'tickets', align: 'right' },
          { title: 'last activity', dataIndex: 'lastAt', render: when },
        ]}
      />
    </>
  )
}

function Conversation({ id, onBack }: { id: string; onBack: () => void }) {
  const { data, error, loading } = useLoad(() => api.conversation(id), [id])

  return (
    <>
      <Space style={{ marginBottom: 8 }}>
        <Button onClick={onBack}>← Conversations</Button>
        <span className="mono">{id}</span>
      </Space>
      {/* Said on the page, not only in the audit table: an operator should know before
          they read, not find out afterwards. */}
      <Typography.Paragraph type="secondary">
        Opening this conversation is recorded in the audit trail.
      </Typography.Paragraph>
      {error && <Alert type="error" showIcon message={error} />}
      {(data?.turns ?? []).map((t) => <TurnCard key={t.id} turn={t} />)}
      {loading && <Card size="small" loading />}
    </>
  )
}

function TurnCard({ turn }: { turn: Turn }) {
  return (
    <Card size="small" style={{ marginBottom: 10 }}>
      <Space wrap size="middle" style={{ marginBottom: 8 }}>
        <Tag color={tagColour(turn.outcome)}>{turn.outcome}</Tag>
        <Typography.Text type="secondary">{when(turn.startedAt)}</Typography.Text>
        {turn.model && <span className="mono">{turn.model}</span>}
        <Typography.Text type="secondary">
          {turn.modelCalls} model call{turn.modelCalls === 1 ? '' : 's'} ·{' '}
          {turn.inputTokens.toLocaleString()} in / {turn.outputTokens.toLocaleString()} out ·{' '}
          {turn.costUsd != null ? `$${turn.costUsd.toFixed(5)}` : 'cost unknown'}
        </Typography.Text>
      </Space>

      <p className="turn-question">{turn.question}</p>
      {turn.reply
        ? <div className="turn-reply"><Markdown text={turn.reply} /></div>
        : <Typography.Text type="secondary">No reply was recorded for this turn.</Typography.Text>}
      {turn.detail && <Alert type="warning" showIcon style={{ marginTop: 8 }} message={turn.detail} />}

      {(turn.passages?.length || turn.toolCalls?.length || turn.traceId) && (
        <Descriptions size="small" column={1} style={{ marginTop: 10 }}>
          {turn.passages?.length ? (
            <Descriptions.Item label="retrieved">
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {turn.passages.map((p) =>
                  `${p.entryId} (${p.language}, ${p.score.toFixed(4)})`).join('  ')}
              </Typography.Text>
            </Descriptions.Item>
          ) : null}
          {turn.toolCalls?.length ? (
            <Descriptions.Item label="tools">
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {turn.toolCalls.map((c) => `${c.name} → ${c.outcome}`).join('  ')}
              </Typography.Text>
            </Descriptions.Item>
          ) : null}
          {turn.traceId ? (
            <Descriptions.Item label="trace">
              <span className="mono">{turn.traceId}</span>
            </Descriptions.Item>
          ) : null}
        </Descriptions>
      )}
    </Card>
  )
}

export function tagColour(outcome: string): string {
  switch (outcome) {
    case 'completed': case 'RESOLVED': return 'green'
    case 'in_flight': case 'IN_PROGRESS': return 'blue'
    case 'cancelled': case 'CLOSED': return 'default'
    case 'OPEN': return 'gold'
    default: return 'red'
  }
}
