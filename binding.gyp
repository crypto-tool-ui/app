{
  "targets": [
    {
      "target_name": "ws_tcp_proxy",
      "sources": ["ws_tcp_proxy.cpp"],
      "include_dirs": [
        "<!(node -e \"require('nan')\")"
      ],
      "conditions": [
        ["OS=='linux'", {
          "cflags_cc": ["-std=c++17", "-O3", "-Wall", "-fexceptions"],
          "libraries": [
            "-lboost_system",
            "-lboost_thread",
            "-lpthread"
          ]
        }],
        ["OS=='mac'", {
          "xcode_settings": {
            "GCC_ENABLE_CPP_EXCEPTIONS": "YES",
            "CLANG_CXX_LIBRARY": "libc++",
            "MACOSX_DEPLOYMENT_TARGET": "10.12",
            "OTHER_CFLAGS": ["-std=c++17", "-O3", "-fexceptions"]
          },
          "libraries": [
            "-lboost_system",
            "-lboost_thread"
          ]
        }],
        ["OS=='win'", {
          "msvs_settings": {
            "VCCLCompilerTool": {
              "ExceptionHandling": 1,
              "AdditionalOptions": ["/std:c++17", "/O2", "/EHsc"]
            }
          },
          "libraries": []
        }]
      ]
    }
  ]
}
