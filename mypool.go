package mypool

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var (
	// ErrClosed 连接池已经关闭
	ErrClosed = errors.New("pool is closed")
	//ErrMaxActiveConnReached 连接池超限
	ErrMaxActiveConnReached = errors.New("MaxActiveConnReached")
)

// Pool 基本方法
type Pool interface {
	// 获取资源
	Get() (interface{}, error)
	// 资源放回去
	Put(interface{}) error
	// 关闭资源
	Close(interface{}) error
	// 释放所有资源
	Release()
	// 当前已有的资源数量
	Len() int
}

// ConnectionFactory 连接工厂
type ConnectionFactory interface {
	//生成连接的方法
	Factory() (interface{}, error)
	//关闭连接的方法
	Close(interface{}) error
	//检查连接是否有效的方法
	Ping(interface{}) error
}

// PoolConfig 连接池相关配置
type PoolConfig struct {
	//连接池中拥有的最小连接数
	InitialCap int

	//最大并发存活连接数
	MaxCap int

	//最大空闲连接
	MaxIdle int //TODO:用途?

	// 工厂
	Factory ConnectionFactory

	//连接最大空闲时间，超过该事件则将失效
	IdleTimeout time.Duration
}

type connReq struct {
	idleConn *idleConn
}

type idleConn struct {
	conn interface{}
	t    time.Time //连接创建的时刻
}

// channelPool 存放连接信息
type channelPool struct {
	mu                       sync.RWMutex
	conns                    chan *idleConn // buffer channel 存储 空闲连接,buffer长度 poolConfig.MaxIdle.               连接数量 一开始为   poolConfig.InitialCap.
	factory                  ConnectionFactory
	idleTimeout, waitTimeOut time.Duration /// 连接空闲超时和等待超时

	maxActive    int // 最大连接数. 起限制作用
	openingConns int // 记录当前打开的连接数量. 初始化为最小连接数

	//	connReqs                 []chan connReq // 连接请求缓冲区，如果无法从 conns 取到连接，则在这个缓冲区创建一个新的元素，之后连接放回去时先填充这个缓冲区  TODO:?
}

// NewChannelPool 初始化连接
func NewChannelPool(poolConfig *PoolConfig) (Pool, error) {
	// 校验参数
	if !(poolConfig.InitialCap <= poolConfig.MaxIdle && poolConfig.MaxCap >= poolConfig.MaxIdle && poolConfig.InitialCap >= 0) {
		return nil, errors.New("invalid capacity settings")
	}
	if poolConfig.Factory == nil {
		return nil, errors.New("invalid factory interface settings")
	}

	c := &channelPool{
		conns:        make(chan *idleConn, poolConfig.MaxIdle),
		factory:      poolConfig.Factory,
		idleTimeout:  poolConfig.IdleTimeout,
		maxActive:    poolConfig.MaxCap,
		openingConns: poolConfig.InitialCap,
	}
	////初始化, 生成 最小连接数 个连接数量. 放在 conns里
	for i := 0; i < poolConfig.InitialCap; i++ {
		conn, err := c.factory.Factory()
		if err != nil {
			c.Release()
			return nil, fmt.Errorf("factory is not able to fill the pool: %s", err)
		}
		c.conns <- &idleConn{conn: conn, t: time.Now()}
	}

	return c, nil
}

// getConns 获取所有连接
func (c *channelPool) getConns() chan *idleConn {
	c.mu.Lock()
	conns := c.conns
	c.mu.Unlock()
	return conns
}

// Get 从pool中取一个连接
func (c *channelPool) Get() (interface{}, error) {
	conns := c.getConns() //获取所有连接
	if conns == nil {     //没有连接 报错
		return nil, ErrClosed
	}
	for {
		select {
		case wrapConn := <-conns:
			if wrapConn == nil {
				return nil, ErrClosed
			}
			//判断是否超时，超时则丢弃
			timeout := c.idleTimeout //空闲时间不为0,才校验
			if timeout > 0 {
				if wrapConn.t.Add(timeout).Before(time.Now()) { //连接创建的时刻+空闲时间 比当前时间小,则该连接闲的时间太久了. 关闭他.
					//丢弃并关闭该连接
					_ = c.Close(wrapConn.conn)
					continue
				}
			}
			//判断是否失效，失效则丢弃，如果用户没有设定 ping 方法，就不检查
			if err := c.Ping(wrapConn.conn); err != nil {
				_ = c.Close(wrapConn.conn)
				continue
			}
			//不超时,也没失效. 则返回该连接.
			return wrapConn.conn, nil

		default: ////TODO: 不停的getConns, 连接都拿完啦.那可怎么办?
			c.mu.Lock()
			log.Printf("openConn %v %v", c.openingConns, c.maxActive)
			if c.openingConns >= c.maxActive { ///当前的连接数已经太多
				return nil, ErrMaxActiveConnReached
				// // 如果达到上限，则创建一个缓冲channel，///在缓冲区里, 等待放回去的连接.
				// req := make(chan connReq, 1)
				// c.connReqs = append(c.connReqs, req)
				// c.mu.Unlock()
				// // 判断是否有连接放回去（放回去逻辑在 put 方法内）
				// ret, ok := <-req
				// // 如果没有连接放回去，则不能再创建新的连接了，因为达到上限了
				// if !ok {
				// 	return nil, ErrMaxActiveConnReached
				// }
				// // 如果有连接放回去了 判断连接是否可用
				// if timeout := c.idleTimeout; timeout > 0 {
				// 	if ret.idleConn.t.Add(timeout).Before(time.Now()) {
				// 		//丢弃并关闭该连接
				// 		// 重新尝试获取连接
				// 		_ = c.Close(ret.idleConn.conn)
				// 		continue
				// 	}
				// }
				// return ret.idleConn.conn, nil
			}

			// 到这里说明 没有空闲连接 && 连接数没有达到上限 可以创建新连接
			if c.factory == nil {
				c.mu.Unlock()
				return nil, ErrClosed
			}
			conn, err := c.factory.Factory()
			if err != nil {
				c.mu.Unlock()
				return nil, err
			}
			c.openingConns++
			c.mu.Unlock()
			return conn, nil
		}
	}
}

// Put 将连接放回pool中
func (c *channelPool) Put(conn interface{}) error {
	if conn == nil {
		return errors.New("connection is nil. rejecting")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conns == nil {
		return c.Close(conn)
	}

	// 如果有请求连接的缓冲区有等待，则按顺序有限个先来的请求分配当前放回的连接
	// if l := len(c.connReqs); l > 0 { ///说明有空位,可以放连接

	// 	req := c.connReqs[0] //把第0位的channel取出来.
	// 	copy(c.connReqs, c.connReqs[1:])
	// 	c.connReqs = c.connReqs[:l-1]

	// 	//放连接进去
	// 	req <- connReq{
	// 		idleConn: &idleConn{conn: conn, t: time.Now()},
	// 	}
	// 	return nil
	// }
	// 如果没有等待的缓冲则尝试放入空闲连接缓冲
	select {
	case c.conns <- &idleConn{conn: conn, t: time.Now()}:
		return nil
	default:
		//连接池已满，直接关闭该连接
		return c.Close(conn)
	}

}

// Close 关闭单条连接
func (c *channelPool) Close(conn interface{}) error {
	if conn == nil {
		return errors.New("connection is nil. rejecting")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openingConns--
	return c.factory.Close(conn)
}

// Ping 检查单条连接是否有效
func (c *channelPool) Ping(conn interface{}) error {
	if conn == nil {
		return errors.New("connection is nil. rejecting")
	}

	return c.factory.Ping(conn)
}

// Release 释放连接池中所有连接
func (c *channelPool) Release() {
	c.mu.Lock()
	conns := c.conns
	c.conns = nil
	c.mu.Unlock()

	defer func() {
		c.factory = nil
	}()

	if conns == nil {
		return
	}

	close(conns)
	for wrapConn := range conns {
		//log.Printf("Type %v\n",reflect.TypeOf(wrapConn.conn))
		_ = c.factory.Close(wrapConn.conn)
	}
}

// Len 连接池中已有的连接数量
func (c *channelPool) Len() int {
	return len(c.getConns())
}
