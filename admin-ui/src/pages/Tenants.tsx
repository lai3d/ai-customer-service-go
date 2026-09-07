import { useState } from 'react'
import {
  Alert, Button, Form, Input, Modal, Popconfirm, Space, Table, Tag, Typography,
} from 'antd'
import { api, ApiError } from '../api/client'
import type { Tenant, TenantKey } from '../api/types'
import { useLoad, when } from './hooks'

// The tenant administration page, and the only page in this application that shows no
// customer content at all.
//
// That is the point of the role it belongs to rather than a property of the layout: an
// account that can create a tenant has no reason to read one, and the account with the
// widest reach in the system is deliberately the one holding the least. The server refuses
// a platform account on every other route, so this page is the whole of what it can do.
export function TenantsPage() {
  const { data, error, loading, reload } = useLoad(() => api.tenants(), [])
  const [creating, setCreating] = useState(false)
  const [keysFor, setKeysFor] = useState<Tenant | null>(null)
  const [problem, setProblem] = useState('')

  const tenants = data?.tenants ?? []

  return (
    <>
      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}

      <Space style={{ marginBottom: 12 }} wrap>
        <Typography.Text type="secondary">
          {tenants.length} {tenants.length === 1 ? 'tenant' : 'tenants'}
        </Typography.Text>
        <Button type="primary" onClick={() => setCreating(true)}>New tenant</Button>
      </Space>

      <Table<Tenant>
        size="small"
        loading={loading}
        rowKey="id"
        dataSource={tenants}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 20, showTotal: (n) => `${n} tenant(s)` }}
        columns={[
          {
            title: 'tenant', dataIndex: 'id', className: 'mono',
            render: (id: string, row) => (
              <Space>
                <span className="mono">{id}</span>
                {row.disabled && <Tag color="red">disabled</Tag>}
              </Space>
            ),
          },
          { title: 'name', dataIndex: 'name' },
          { title: 'created', dataIndex: 'createdAt', render: when },
          { title: 'by', dataIndex: 'createdBy' },
          {
            title: '', key: 'actions', width: 260,
            render: (_: unknown, row: Tenant) => (
              <Space>
                <Button size="small" onClick={() => setKeysFor(row)}>Keys</Button>
                <Popconfirm
                  title={row.disabled ? 'Enable this tenant?' : 'Disable this tenant?'}
                  description={row.disabled
                    ? 'Its API keys start working again immediately.'
                    : 'Every one of its API keys stops working immediately. Its data is untouched.'}
                  okText={row.disabled ? 'Enable' : 'Disable'}
                  onConfirm={async () => {
                    try {
                      await api.setTenantDisabled(row.id, !row.disabled)
                      void reload()
                    } catch (err) {
                      setProblem(err instanceof ApiError ? err.message : String(err))
                    }
                  }}
                >
                  <Button size="small" danger={!row.disabled}>
                    {row.disabled ? 'Enable' : 'Disable'}
                  </Button>
                </Popconfirm>
              </Space>
            ),
          },
        ]}
      />

      <Typography.Paragraph type="secondary" style={{ marginTop: 16, fontSize: 12 }}>
        Disabling stops a tenant's keys answering and touches none of its rows. Cancelling an
        account and erasing what its customers said are two decisions, and this page only
        makes the first one.
      </Typography.Paragraph>

      {creating && (
        <NewTenant
          onClose={() => setCreating(false)}
          onCreated={() => { setCreating(false); void reload() }}
        />
      )}
      {keysFor && <Keys tenant={keysFor} onClose={() => setKeysFor(null)} />}
    </>
  )
}

function NewTenant(props: { onClose: () => void; onCreated: () => void }) {
  const [id, setId] = useState('')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState('')

  return (
    <Modal
      open
      title="New tenant"
      okText="Create"
      confirmLoading={busy}
      onCancel={props.onClose}
      onOk={async () => {
        setBusy(true)
        setProblem('')
        try {
          await api.createTenant(id.trim(), name.trim())
          props.onCreated()
        } catch (err) {
          setProblem(err instanceof ApiError ? err.message : String(err))
        } finally {
          setBusy(false)
        }
      }}
    >
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}
      <Form layout="vertical">
        <Form.Item
          label="Tenant id"
          help="Lowercase letters, digits and hyphens. It appears in log lines, in metric labels and in URLs, so it is chosen rather than generated — and it cannot be changed later."
        >
          <Input value={id} onChange={(e) => setId(e.target.value)} placeholder="acme" />
        </Form.Item>
        <Form.Item label="Name" help="What a person calls this customer.">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Acme Ltd" />
        </Form.Item>
      </Form>
      <Alert
        type="info"
        showIcon
        message="A new tenant starts with nothing."
        description="No corpus, no knowledge entries. Until somebody publishes to it, its assistant answers every question by saying it does not know and offering a human — which is the correct behaviour, not a fault to report."
      />
    </Modal>
  )
}

// The keys of one tenant.
//
// `issued` is the whole reason this is a modal with state rather than a table row: the
// secret exists in exactly one response and is stored nowhere, so if this page loses it
// before somebody copies it, the only remedy is to revoke it and issue another.
function Keys({ tenant, onClose }: { tenant: Tenant; onClose: () => void }) {
  const { data, error, loading, reload } = useLoad(() => api.tenantKeys(tenant.id), [tenant.id])
  const [label, setLabel] = useState('')
  const [issued, setIssued] = useState<TenantKey | null>(null)
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState('')

  const keys = data?.keys ?? []

  return (
    <Modal open width={860} title={`Keys · ${tenant.id}`} onCancel={onClose} footer={null}>
      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}

      {issued && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message="Copy this key now. It is not stored and cannot be shown again."
          description={
            <>
              <div className="mono" style={{ wordBreak: 'break-all', margin: '6px 0' }}>
                {issued.secret}
              </div>
              <Typography.Text type="secondary">
                It goes in the <span className="mono">X-API-Key</span> header, not in
                {' '}<span className="mono">Authorization</span> — that one carries the
                customer's session token.
              </Typography.Text>
            </>
          }
        />
      )}

      <Space style={{ marginBottom: 12 }} wrap>
        <Input
          style={{ width: 300 }}
          placeholder="what this key is for"
          value={label}
          onChange={(e) => setLabel(e.target.value)}
        />
        <Button
          type="primary"
          loading={busy}
          disabled={label.trim() === ''}
          onClick={async () => {
            setBusy(true)
            setProblem('')
            try {
              setIssued(await api.issueTenantKey(tenant.id, label.trim()))
              setLabel('')
              void reload()
            } catch (err) {
              setProblem(err instanceof ApiError ? err.message : String(err))
            } finally {
              setBusy(false)
            }
          }}
        >
          Issue a key
        </Button>
        <Typography.Text type="secondary">
          A label is required: revoking the right key later should not be a guess.
        </Typography.Text>
      </Space>

      <Table<TenantKey>
        size="small"
        loading={loading}
        rowKey="keyId"
        dataSource={keys}
        scroll={{ x: 'max-content' }}
        pagination={false}
        columns={[
          {
            title: 'key id', dataIndex: 'keyId', className: 'mono',
            render: (id: string, row) => (
              <Space>
                <span className="mono">{id}</span>
                {row.revokedAt && <Tag color="red">revoked</Tag>}
              </Space>
            ),
          },
          { title: 'label', dataIndex: 'label' },
          { title: 'issued', dataIndex: 'createdAt', render: when },
          { title: 'by', dataIndex: 'createdBy' },
          {
            title: 'last used', dataIndex: 'lastUsedAt',
            render: (at?: string) => at
              ? when(at)
              : <Typography.Text type="secondary">never</Typography.Text>,
          },
          {
            title: '', key: 'revoke', width: 100,
            render: (_: unknown, row: TenantKey) => row.revokedAt ? null : (
              <Popconfirm
                title="Revoke this key?"
                description="Whatever is using it stops working immediately, and it cannot be un-revoked."
                okText="Revoke"
                onConfirm={async () => {
                  try {
                    await api.revokeTenantKey(tenant.id, row.keyId)
                    void reload()
                  } catch (err) {
                    setProblem(err instanceof ApiError ? err.message : String(err))
                  }
                }}
              >
                <Button size="small" danger>Revoke</Button>
              </Popconfirm>
            ),
          },
        ]}
      />

      <Typography.Paragraph type="secondary" style={{ marginTop: 12, fontSize: 12 }}>
        A revoked key keeps its row. Which key was revoked, when, and what it was for is the
        question somebody asks after an incident.
      </Typography.Paragraph>
    </Modal>
  )
}
