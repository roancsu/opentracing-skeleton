package main
 
import (
    "context"
    "log"
    "net"
    "io"
    opentracing "github.com/opentracing/opentracing-go"
    jaeger "github.com/uber/jaeger-client-go"
    "github.com/uber/jaeger-client-go/config"
    metadata "google.golang.org/grpc/metadata"
    "google.golang.org/grpc"
    pb "src/opentrace/grpc/helloworld/output/github.com/grpc/example/helloworld"
    "google.golang.org/grpc/reflection"
    logger "github.com/roancsu/traceandtrace-go/libs/log"
    "fmt"
    "time"
)
 
const (
    port = ":50050"
    addr     = "localhost:50051"
    serviceName = "rpc:server:client"
    traceAgentHost = "127.0.0.1:6831"
)

var tracer opentracing.Tracer
var ctxShare context.Context
var rpcCtx string
 
// server is used to implement helloworld.GreeterServer.
type server struct{}
 
// SayHello implements helloworld.GreeterServer
func (s *server) SayHello(ctx context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
    return &pb.HelloReply{Message: "Hello " + in.Name}, nil
}


func initJaeger(service string, jaegerAgentHost string) (tracer opentracing.Tracer, closer io.Closer, err error) {
    cfg := &config.Configuration{
        Sampler: &config.SamplerConfig{
            Type:  "const",
            Param: 1,
        },
        Reporter: &config.ReporterConfig{
            LogSpans: true,
            LocalAgentHostPort:jaegerAgentHost,
        },
    }
    tracer, closer, err = cfg.New(service, config.Logger(jaeger.StdLogger))
    opentracing.SetGlobalTracer(tracer)
    return tracer, closer, err
}



func serverOption(tracer opentracing.Tracer) grpc.ServerOption {
    return grpc.UnaryInterceptor(jaegerGrpcServerInterceptor)
}

 
type TextMapReader struct {
    metadata.MD
}

type TextMapWriter struct {
    metadata.MD
}


func (t TextMapWriter) Set(key, val string) {
    //key = strings.ToLower(key)
    t.MD[key] = append(t.MD[key], val)
}


//读取metadata中的span信息
func (t TextMapReader) ForeachKey(handler func(key, val string) error) error { //不能是指针
    for key, val := range t.MD {
        for _, v := range val {
            if err := handler(key, v); err != nil {
                return err
            }
        }
    }
    return nil
}


func clientDialOption(parentTracer opentracing.Tracer) grpc.DialOption {
    tracer = parentTracer
    return grpc.WithUnaryInterceptor(jaegerGrpcClientInterceptor)
}


func jaegerGrpcServerInterceptor(
    ctx context.Context, 
    req interface{}, 
    info *grpc.UnaryServerInfo, 
    handler grpc.UnaryHandler) (resp interface{}, err error) {
    //从context中获取metadata。md.(type) == map[string][]string
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        md = metadata.New(nil)
    } else {
        //如果对metadata进行修改，那么需要用拷贝的副本进行修改。（FromIncomingContext的注释）
        md = md.Copy()
    }
    carrier := TextMapReader{md}
    tracer := opentracing.GlobalTracer()
    spanContext, e := tracer.Extract(opentracing.TextMap, carrier)
    if e != nil {
        fmt.Println("Extract err:", e)
    }
 
    span := tracer.StartSpan(info.FullMethod, opentracing.ChildOf(spanContext))
    defer span.Finish()
    fmt.Println(span)
    ctx = opentracing.ContextWithSpan(ctx, span)
    rpcCtx = serviceName+"ctx"
    ctxShare = context.WithValue(context.Background(), rpcCtx, opentracing.ContextWithSpan(context.Background(), span))
    rpcRequest(tracer)
 
    return handler(ctx, req)
}


func jaegerGrpcClientInterceptor (
    ctx context.Context, 
    method string, 
    req, reply interface{},
    cc *grpc.ClientConn, 
    invoker grpc.UnaryInvoker, 
    opts ...grpc.CallOption) (err error) {

    if rpcCtx != "" {
        if v := ctx.Value(rpcCtx); v == nil {
            ctx = ctxShare.Value(rpcCtx).(context.Context)
            logger.Info(fmt.Sprintf("trace rpc parent ctx ... %v\n", ctx))
        }
    }

    //从context中获取metadata。md.(type) == map[string][]string
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        md = metadata.New(nil)
    } else {
        //如果对metadata进行修改，那么需要用拷贝的副本进行修改。（FromIncomingContext的注释）
        md = md.Copy()
    }
    //定义一个carrier，下面的Inject注入数据需要用到。carrier.(type) == map[string]string
    //carrier := opentracing.TextMapCarrier{}
    carrier := TextMapWriter{md}

    var currentContext opentracing.SpanContext
    //从context中获取原始的span
    parentSpan := opentracing.SpanFromContext(ctx)
    if parentSpan != nil {
        currentContext = parentSpan.Context()
    }else{
        //start span
        span := tracer.StartSpan(method)
        defer span.Finish()
        currentContext = span.Context()
    }

    //将span的context信息注入到carrier中
    e := tracer.Inject(currentContext, opentracing.TextMap, carrier)
    if e != nil {
        fmt.Println("tracer Inject err,", e)
    }
    //创建一个新的context，把metadata附带上
    ctx = metadata.NewOutgoingContext(ctx, md)
 
    return invoker(ctx, method, req, reply, cc, opts...)
}



func rpcRequest(tracer opentracing.Tracer) {
    // dial
    conn, err := grpc.Dial(addr, grpc.WithInsecure(), clientDialOption(tracer))
    if err != nil {
    }
    //发送请求
    name := "ethan"
    ctx, _ := context.WithTimeout(context.Background(), time.Second)
    c := pb.NewGreeterClient(conn)
    r, err := c.SayHello(ctx, &pb.HelloRequest{Name: name})
    if err != nil {
        logger.Error(fmt.Sprintf("could not greet %s", err))
    }
    fmt.Println("Greeting: %s", r.Message)
}



func main() {
    fmt.Println("rpc server start ...")
    tracer, closer, err := initJaeger(serviceName, "127.0.0.1:6831")
    if err != nil {
        // log.Fatal(err)
        fmt.Println("init jaeger err", err)
    }
    defer closer.Close()

    lis, err := net.Listen("tcp", port)
    if err != nil {
        log.Fatalf("failed to listen: %v", err)
    }
    opts := serverOption(tracer)
    s := grpc.NewServer(opts)
    pb.RegisterGreeterServer(s, &server{})
    // Register reflection service on gRPC server.
    reflection.Register(s)
    if err := s.Serve(lis); err != nil {
        log.Fatalf("failed to serve: %v", err)
    }

    fmt.Println("okk...")
}
 







