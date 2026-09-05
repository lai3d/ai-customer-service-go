import { useState } from 'react'
import {
  Alert, Button, Descriptions, Form, Input, Modal, Select, Space, Table, Tag, Timeline, Typography,
} from 'antd'
import { api, ApiError } from '../api/client'
import type { Ticket, TicketEvent, TicketState } from '../api/types'
import { NEXT_STATES } from '../api/types'
import { useLoad, when } from './hooks'
import { tagColour } from './Conversations'

const STATES: TicketState[] = ['OPEN', 'IN_PROGRESS', 'RESOLVED', 'CLOSED']

export function TicketsPage({ canWrite }: { canWrite: boolean }) {
  const [state, setState] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const { data, error, loading, reload } = useLoad(
    () => api.tickets({ state, limit: 200 }), [state])

  return (
    <>
      <Space style={{ marginBottom: 12 }} wrap>
        <Select
          style={{ width: 200 }}
          value={state}
          onChange={setState}
          options={[{ value: '', label: 'any state' }, ...STATES.map((s) => ({ value: s, label: s }))]}
        />
        {!canWrite && <Typography.Text type="secondary">You have read-only access.</Typography.Text>}
      </Space>

      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}

      <Table<Ticket>
        size="small"
        loading={loading}
        rowKey="ticketNumber"
        dataSource={data?.tickets ?? []}
        onRow={(row) => ({ onClick: () => setOpen(row.ticketNumber), style: { cursor: 'pointer' } })}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 20, showTotal: (n) => `${n} ticket(s)` }}
        columns={[
          { title: 'ticket', dataIndex: 'ticketNumber', className: 'mono' },
          {
            title: 'state', dataIndex: 'state',
            render: (s: string) => <Tag color={tagColour(s)}>{s}</Tag>,
          },
          { title: 'assignee', dataIndex: 'assignee', render: (a?: string) => a ?? '—' },
          { title: 'summary', dataIndex: 'summary', width: 420 },
          { title: 'conversation', dataIndex: 'conversationId', className: 'mono' },
          { title: 'updated', dataIndex: 'updatedAt', render: when },
        ]}
      />

      {open && (
        <TicketDialog
          number={open}
          canWrite={canWrite}
          onClose={() => setOpen(null)}
          onSaved={() => { setOpen(null); void reload() }}
        />
      )}
    </>
  )
}

function TicketDialog(props: {
  number: string
  canWrite: boolean
  onClose: () => void
  onSaved: () => void
}) {
  const { data, error, loading, reload } = useLoad(() => api.ticket(props.number), [props.number])
  const [problem, setProblem] = useState('')
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm()

  const ticket = data?.ticket
  const history: TicketEvent[] = data?.history ?? []

  const save = async () => {
    if (!ticket) return
    setSaving(true)
    setProblem('')
    const values = form.getFieldsValue()
    try {
      await api.updateTicket(ticket.ticketNumber, {
        expectedVersion: ticket.version,
        state: values.state || undefined,
        // Sent only when it changed. The server treats a present assignee as an
        // assignment, so resending the same one writes an event saying nothing happened.
        assignee: (values.assignee ?? '') !== (ticket.assignee ?? '')
          ? (values.assignee ?? '') : undefined,
        resolution: values.resolution?.trim() || undefined,
        reason: values.reason?.trim() || undefined,
        note: values.note?.trim() || undefined,
      })
      props.onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        // Someone else changed this ticket while it was open. Reloading is the fix, and
        // saying so is the difference between a usable page and a mysterious one.
        setProblem(err.message + ' — reloading the ticket.')
        void reload()
      } else {
        setProblem(err instanceof ApiError ? err.message : String(err))
      }
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      open
      width={720}
      title={ticket
        ? <Space><span className="mono">{ticket.ticketNumber}</span>
            <Tag color={tagColour(ticket.state)}>{ticket.state}</Tag>
            <Typography.Text type="secondary">v{ticket.version}</Typography.Text></Space>
        : props.number}
      onCancel={props.onClose}
      confirmLoading={saving}
      footer={props.canWrite
        ? [<Button key="c" onClick={props.onClose}>Close</Button>,
           <Button key="s" type="primary" loading={saving} onClick={save}>Save</Button>]
        : [<Button key="c" onClick={props.onClose}>Close</Button>]}
    >
      {error && <Alert type="error" showIcon message={error} />}
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}
      {loading && !ticket && <Typography.Text type="secondary">Loading…</Typography.Text>}

      {ticket && (
        <>
          <Descriptions size="small" column={1} style={{ marginBottom: 12 }}>
            <Descriptions.Item label="summary">{ticket.summary}</Descriptions.Item>
            <Descriptions.Item label="category">{ticket.category}</Descriptions.Item>
            <Descriptions.Item label="conversation">
              <span className="mono">{ticket.conversationId}</span>
            </Descriptions.Item>
            {ticket.orderNumber && (
              <Descriptions.Item label="order">{ticket.orderNumber}</Descriptions.Item>
            )}
          </Descriptions>

          {props.canWrite && (
            <Form
              form={form}
              layout="vertical"
              // The resolution box is deliberately NOT filled from the row. It was, and
              // an operator reopening a ticket resubmitted the old conclusion without
              // touching it -- found by opening this dialog in a browser. The conclusion
              // a ticket has already been given is in the history below.
              initialValues={{ state: '', assignee: ticket.assignee ?? '' }}
            >
              <Form.Item name="state" label="Move to">
                <Select
                  options={[
                    { value: '', label: `leave as ${ticket.state}` },
                    ...NEXT_STATES[ticket.state].map((s) => ({ value: s, label: s })),
                  ]}
                />
              </Form.Item>
              <Form.Item name="assignee" label="Assignee (blank to unassign)">
                <Input />
              </Form.Item>
              <Form.Item name="resolution" label="Resolution — required to resolve">
                <Input.TextArea rows={2} />
              </Form.Item>
              <Form.Item name="reason" label="Reason — required to reopen">
                <Input.TextArea rows={2} />
              </Form.Item>
              <Form.Item name="note" label="Note">
                <Input.TextArea rows={2} />
              </Form.Item>
            </Form>
          )}

          <Typography.Title level={5}>History</Typography.Title>
          <Timeline
            items={history.map((e) => ({
              children: (
                <>
                  <Typography.Text type="secondary">{when(e.at)} · </Typography.Text>
                  <Typography.Text strong>{e.actor} {e.action}</Typography.Text>
                  {e.detail && <Typography.Text type="secondary"> — {e.detail}</Typography.Text>}
                </>
              ),
            }))}
          />
        </>
      )}
    </Modal>
  )
}
