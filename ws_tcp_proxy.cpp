#include <node.h>
#include <boost/asio.hpp>
#include <boost/beast.hpp>
#include <boost/beast/websocket.hpp>
#include <boost/thread.hpp>
#include <memory>
#include <string>
#include <atomic>
#include <queue>
#include <mutex>

namespace beast = boost::beast;
namespace websocket = beast::websocket;
namespace net = boost::asio;
using tcp = net::ip::tcp;

// Base64 decoder
std::string base64_decode(const std::string& encoded) {
    static const std::string b64_chars = 
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    
    std::string decoded;
    std::vector<int> T(256, -1);
    for (int i = 0; i < 64; i++) T[b64_chars[i]] = i;
    
    int val = 0, valb = -8;
    for (unsigned char c : encoded) {
        if (T[c] == -1) break;
        val = (val << 6) + T[c];
        valb += 6;
        if (valb >= 0) {
            decoded.push_back(char((val >> valb) & 0xFF));
            valb -= 8;
        }
    }
    return decoded;
}

// Parse TCP address from base64
std::pair<std::string, std::string> parse_tcp_address(const std::string& b64, bool& success) {
    success = false;
    std::string decoded = base64_decode(b64);
    size_t colon = decoded.find(':');
    if (colon == std::string::npos) {
        return {"", ""};
    }
    success = true;
    return {decoded.substr(0, colon), decoded.substr(colon + 1)};
}

// Session handles bidirectional proxy between WebSocket and TCP
class ProxySession : public std::enable_shared_from_this<ProxySession> {
    websocket::stream<beast::tcp_stream> ws_;
    tcp::socket tcp_socket_;
    beast::flat_buffer ws_buffer_;
    std::vector<char> tcp_buffer_;
    std::atomic<bool> closed_{false};
    
    static constexpr size_t BUFFER_SIZE = 4096; // Giảm từ 8192 để tiết kiệm RAM

public:
    ProxySession(tcp::socket socket, net::io_context& ioc)
        : ws_(std::move(socket))
        , tcp_socket_(ioc)
        , tcp_buffer_(BUFFER_SIZE) {
        // Tối ưu buffer limits
        ws_buffer_.max_size(BUFFER_SIZE * 2);
    }

    void run(const std::string& target) {
        // Tối ưu timeout cho high concurrency
        ws_.set_option(websocket::stream_base::timeout{
            std::chrono::seconds(30),    // handshake timeout
            std::chrono::seconds(300),   // idle timeout (5 phút)
            false                         // keep alive pings
        });
        
        ws_.set_option(websocket::stream_base::decorator([](websocket::response_type& res) {
            res.set(beast::http::field::server, "ws-tcp-proxy");
        }));
        
        // Giảm buffer size cho WebSocket frame
        ws_.read_message_max(BUFFER_SIZE * 4); // Max 16KB message

        auto self = shared_from_this();
        ws_.async_accept(
            [self, target](beast::error_code ec) {
                if (ec) return;
                self->connect_tcp(target);
            });
    }

private:
    void connect_tcp(const std::string& target) {
        bool success = false;
        auto result = parse_tcp_address(target, success);
        if (!success) {
            close();
            return;
        }
        
        auto host = result.first;
        auto port = result.second;
        tcp::resolver resolver(tcp_socket_.get_executor());
        
        beast::error_code ec;
        auto endpoints = resolver.resolve(host, port, ec);
        if (ec) {
            close();
            return;
        }
        
        auto self = shared_from_this();
        net::async_connect(tcp_socket_, endpoints,
            [self](beast::error_code ec, tcp::endpoint) {
                if (ec) {
                    self->close();
                    return;
                }
                self->start_proxy();
            });
    }

    void start_proxy() {
        read_websocket();
        read_tcp();
    }

    void read_websocket() {
        auto self = shared_from_this();
        ws_.async_read(ws_buffer_,
            [self](beast::error_code ec, std::size_t bytes) {
                if (ec) {
                    self->close();
                    return;
                }
                
                auto data = net::buffer_cast<const char*>(self->ws_buffer_.data());
                net::async_write(self->tcp_socket_, 
                    net::buffer(data, bytes),
                    [self](beast::error_code ec, std::size_t) {
                        self->ws_buffer_.consume(self->ws_buffer_.size());
                        if (!ec) self->read_websocket();
                        else self->close();
                    });
            });
    }

    void read_tcp() {
        auto self = shared_from_this();
        tcp_socket_.async_read_some(net::buffer(tcp_buffer_),
            [self](beast::error_code ec, std::size_t bytes) {
                if (ec) {
                    self->close();
                    return;
                }
                
                self->ws_.async_write(net::buffer(self->tcp_buffer_.data(), bytes),
                    [self](beast::error_code ec, std::size_t) {
                        if (!ec) self->read_tcp();
                        else self->close();
                    });
            });
    }

    void close() {
        if (closed_.exchange(true)) return;
        
        beast::error_code ec;
        ws_.close(websocket::close_code::normal, ec);
        tcp_socket_.close(ec);
    }
};

// Listener accepts WebSocket connections
class Listener : public std::enable_shared_from_this<Listener> {
    net::io_context& ioc_;
    tcp::acceptor acceptor_;

public:
    Listener(net::io_context& ioc, tcp::endpoint endpoint)
        : ioc_(ioc), acceptor_(ioc) {
        acceptor_.open(endpoint.protocol());
        acceptor_.set_option(net::socket_base::reuse_address(true));
        
        // Tối ưu cho high concurrency
        acceptor_.set_option(tcp::acceptor::reuse_address(true));
        acceptor_.set_option(tcp::no_delay(true)); // Disable Nagle
        
        acceptor_.bind(endpoint);
        acceptor_.listen(net::socket_base::max_listen_connections);
    }

    void run() {
        accept();
    }

private:
    void accept() {
        acceptor_.async_accept(net::make_strand(ioc_),
            [self = shared_from_this()](beast::error_code ec, tcp::socket socket) {
                if (!ec) {
                    self->handle_connection(std::move(socket));
                }
                self->accept();
            });
    }

    void handle_connection(tcp::socket socket) {
        // Read HTTP request to extract target from path
        auto stream = std::make_shared<beast::tcp_stream>(std::move(socket));
        auto buffer = std::make_shared<beast::flat_buffer>();
        auto req = std::make_shared<beast::http::request<beast::http::string_body>>();
        
        beast::http::async_read(*stream, *buffer, *req,
            [this, stream, buffer, req](beast::error_code ec, std::size_t) {
                if (ec) return;
                
                std::string target = std::string(req->target());
                if (target.size() > 1 && target[0] == '/') {
                    target = target.substr(1);
                }
                
                auto session = std::make_shared<ProxySession>(
                    stream->release_socket(), ioc_);
                session->run(target);
            });
    }
};

// Proxy server with thread pool
class ProxyServer {
    std::vector<std::shared_ptr<net::io_context>> io_contexts_;
    std::vector<std::shared_ptr<net::io_context::work>> work_guards_;
    boost::thread_group thread_pool_;
    std::shared_ptr<Listener> listener_;
    std::atomic<bool> running_{false};

public:
    bool start(const std::string& host, int port, int threads = 0) {
        if (running_.exchange(true)) return false;
        
        if (threads <= 0) {
            threads = std::max(2u, boost::thread::hardware_concurrency());
        }

        // Create io_context pool
        for (int i = 0; i < threads; ++i) {
            auto ioc = std::make_shared<net::io_context>();
            auto work = std::make_shared<net::io_context::work>(*ioc);
            io_contexts_.push_back(ioc);
            work_guards_.push_back(work);
        }

        // Start listener on first io_context
        beast::error_code ec;
        auto addr = net::ip::make_address(host, ec);
        if (ec) {
            running_ = false;
            return false;
        }
        
        auto endpoint = tcp::endpoint(addr, port);
        listener_ = std::make_shared<Listener>(*io_contexts_[0], endpoint);
        listener_->run();

        // Start thread pool
        for (auto& ioc : io_contexts_) {
            thread_pool_.create_thread([ioc]() {
                ioc->run();
            });
        }
        
        return true;
    }

    void stop() {
        if (!running_.exchange(false)) return;
        
        work_guards_.clear();
        for (auto& ioc : io_contexts_) {
            ioc->stop();
        }
        thread_pool_.join_all();
        io_contexts_.clear();
    }

    bool is_running() const {
        return running_.load();
    }
};

// Node.js addon bindings
static std::unique_ptr<ProxyServer> g_server;

void Start(const v8::FunctionCallbackInfo<v8::Value>& args) {
    v8::Isolate* isolate = args.GetIsolate();
    
    if (args.Length() < 2) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8(isolate, "Wrong number of arguments").ToLocalChecked()));
        return;
    }

    v8::String::Utf8Value host(isolate, args[0]);
    int port = args[1]->Int32Value(isolate->GetCurrentContext()).FromJust();
    int threads = args.Length() > 2 ? 
        args[2]->Int32Value(isolate->GetCurrentContext()).FromJust() : 0;

    if (!g_server) {
        g_server = std::make_unique<ProxyServer>();
    }
    
    bool started = g_server->start(*host, port, threads);
    if (!started) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8(isolate, "Failed to start proxy server").ToLocalChecked()));
        return;
    }
    
    args.GetReturnValue().Set(v8::Boolean::New(isolate, true));
}

void Stop(const v8::FunctionCallbackInfo<v8::Value>& args) {
    if (g_server) {
        g_server->stop();
    }
    args.GetReturnValue().Set(v8::Boolean::New(args.GetIsolate(), true));
}

void IsRunning(const v8::FunctionCallbackInfo<v8::Value>& args) {
    bool running = g_server && g_server->is_running();
    args.GetReturnValue().Set(v8::Boolean::New(args.GetIsolate(), running));
}

void Initialize(v8::Local<v8::Object> exports) {
    NODE_SET_METHOD(exports, "start", Start);
    NODE_SET_METHOD(exports, "stop", Stop);
    NODE_SET_METHOD(exports, "isRunning", IsRunning);
}

NODE_MODULE(ws_tcp_proxy, Initialize)
