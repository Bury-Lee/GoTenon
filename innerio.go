package GoTenon

//组件间通信功能
//满足该功能的组件可以在需要时向内核申请与其他组件的独立通信空间
type InnerIO interface {
	DealWith(From string, Message Message) error //输入值,处理事件,返回结果
	SendTo(Des string, Message Message) error    //向目标发送信号和数据
}
