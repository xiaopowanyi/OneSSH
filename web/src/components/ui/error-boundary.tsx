import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'

type Props = { children: ReactNode }
type State = { error: Error | null }

/**
 * 路由级错误边界。懒加载的页面 chunk 拉取失败、或渲染时抛出异常，
 * React 默认会卸载整棵树变成一片空白——用户既看不到原因也没有出路。
 * 这里兜住它，给出可恢复的界面：重试会回到应用内，刷新则重新拉取资源。
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('页面渲染失败:', error, info.componentStack)
  }

  private retry = () => this.setState({ error: null })

  render() {
    const { error } = this.state
    if (!error) return this.props.children
    return (
      <div className="grid min-h-[60dvh] place-items-center px-6">
        <div className="max-w-md text-center">
          <h1 className="text-base font-medium">页面出错了</h1>
          <p className="mt-2 text-sm text-muted">
            {error.message || '渲染该页面时抛出了异常'}
          </p>
          <div className="mt-5 flex justify-center gap-2">
            <Button variant="primary" onClick={this.retry}>
              重试
            </Button>
            <Button variant="outline" onClick={() => window.location.reload()}>
              刷新页面
            </Button>
          </div>
        </div>
      </div>
    )
  }
}
