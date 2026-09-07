import { useCallback, useEffect, useState } from 'react'
import { Alert, App as AntApp, Button, ConfigProvider, Form, Input, Layout, Tabs, Typography } from 'antd'
import { api, ApiError, apiBase, setToken, token } from './api/client'
import type { WhoAmI } from './api/types'
import { OverviewPage } from './pages/Overview'
import { ConversationsPage } from './pages/Conversations'
import { TicketsPage } from './pages/Tickets'
import { AuditPage } from './pages/Audit'
import { KnowledgePage } from './pages/Knowledge'

export function App() {
  const [me, setMe] = useState<WhoAmI | null>(null)
  const [problem, setProblem] = useState('')
  const [checking, setChecking] = useState(true)

  const identify = useCallback(async () => {
    if (!token()) {
      setMe(null)
      setChecking(false)
      return
    }
    try {
      setMe(await api.whoami())
      setProblem('')
    } catch (err) {
      setMe(null)
      setProblem(err instanceof ApiError ? err.message : String(err))
    } finally {
      setChecking(false)
    }
  }, [])

  useEffect(() => {
    void identify()
  }, [identify])

  return (
    <ConfigProvider theme={{ token: { colorPrimary: '#0f766e' } }}>
      <AntApp>
        {me ? (
          <Signed me={me} onSignOut={() => { setToken(''); setMe(null) }} />
        ) : (
          <SignIn problem={problem} checking={checking} onSignedIn={identify} />
        )}
      </AntApp>
    </ConfigProvider>
  )
}

function SignIn(props: { problem: string; checking: boolean; onSignedIn: () => void }) {
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)

  return (
    <Layout style={{ minHeight: '100vh', alignItems: 'center', justifyContent: 'center' }}>
      <div style={{ width: 460, padding: 24 }}>
        <Typography.Title level={4} style={{ marginTop: 0 }}>Operations</Typography.Title>
        <Typography.Paragraph type="secondary">
          This page shows what customers wrote. Every conversation you open is recorded in
          the audit trail with your name.
        </Typography.Paragraph>
        {props.problem && (
          <Alert type="error" showIcon style={{ marginBottom: 12 }} message={props.problem} />
        )}
        <Form
          layout="vertical"
          onFinish={async () => {
            setBusy(true)
            setToken(value.trim())
            props.onSignedIn()
            setBusy(false)
          }}
        >
          <Form.Item label="Operator token">
            <Input.Password
              autoComplete="off"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder="bearer token"
            />
          </Form.Item>
          <Button type="primary" htmlType="submit" loading={busy || props.checking}>
            Sign in
          </Button>
        </Form>
        <Typography.Paragraph type="secondary" style={{ marginTop: 16, fontSize: 12 }}>
          API: <span className="mono">{apiBase() || '(same origin)'}</span>
        </Typography.Paragraph>
      </div>
    </Layout>
  )
}

function Signed({ me, onSignOut }: { me: WhoAmI; onSignOut: () => void }) {
  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Layout.Header
        style={{
          background: '#fff', borderBottom: '1px solid #f0f0f0', display: 'flex',
          alignItems: 'center', justifyContent: 'space-between', paddingInline: 20,
        }}
      >
        <Typography.Text strong>Operations</Typography.Text>
        <span>
          <Typography.Text type="secondary" style={{ marginRight: 12 }}>
            {me.name} · {me.role}
          </Typography.Text>
          <Button size="small" onClick={onSignOut}>Sign out</Button>
        </span>
      </Layout.Header>
      <Layout.Content style={{ padding: 20 }}>
        <Tabs
          defaultActiveKey="overview"
          destroyInactiveTabPane
          items={[
            { key: 'overview', label: 'Overview', children: <OverviewPage /> },
            { key: 'conversations', label: 'Conversations', children: <ConversationsPage /> },
            { key: 'tickets', label: 'Tickets', children: <TicketsPage canWrite={me.canWrite} /> },
            { key: 'knowledge', label: 'Knowledge', children: <KnowledgePage canWrite={me.canWrite} /> },
            { key: 'audit', label: 'Audit', children: <AuditPage /> },
          ]}
        />
      </Layout.Content>
    </Layout>
  )
}
