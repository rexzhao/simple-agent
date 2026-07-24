import { useState } from 'react'
import { ChatIcon, FolderIcon, LogoIcon, PlusIcon } from './icons'

export function ProjectSetup(props: { suggestedRoot: string; hasProjects: boolean; onCancel: () => void; onSubmit: (root: string, name: string) => void }) {
  const [root, setRoot] = useState(props.suggestedRoot)
  const [name, setName] = useState('')
  return (
    <div className="setup-screen">
      <div className="setup-card">
        <div className="setup-icon"><FolderIcon /></div>
        <span className="eyebrow">本地工作区</span>
        <h1>{props.hasProjects ? '添加另一个项目' : '连接你的第一个项目'}</h1>
        <p>项目目录只作为工作区；Provider、模型和运行配置由当前 Server Root 统一管理。</p>
        <label>项目目录<input value={root} onChange={(event) => setRoot(event.target.value)} placeholder="F:\work\project" autoFocus /></label>
        <label>显示名称 <small>可选</small><input value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：Simple Agent" /></label>
        <div className="setup-actions">
          {props.hasProjects && <button className="secondary-button" onClick={props.onCancel}>取消</button>}
          <button className="primary-button" disabled={!root.trim()} onClick={() => props.onSubmit(root.trim(), name.trim())}>连接项目</button>
        </div>
      </div>
    </div>
  )
}

export function EmptySession({ disabled, onCreate }: { disabled: boolean; onCreate: () => void }) {
  return <div className="setup-screen"><div className="empty-session"><ChatIcon /><h1>还没有会话</h1><p>创建会话后即可开始与项目中的 Agent 协作。</p><button className="primary-button" disabled={disabled} onClick={onCreate}><PlusIcon /> 新建会话</button></div></div>
}

export function ErrorBanner({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return <div className="error-banner" role="alert"><span>{message}</span><button onClick={onDismiss} aria-label="关闭">×</button></div>
}

export function Splash() {
  return <div className="splash"><div className="splash-logo"><LogoIcon /></div><span>SAI</span></div>
}

export function MessageSkeleton() {
  return <div className="skeleton"><i /><i /><i /></div>
}
