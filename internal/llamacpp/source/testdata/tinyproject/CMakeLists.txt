# A llama.cpp-shaped project small enough to compile in a second.
#
# It exists to prove the things a fake `cmake` cannot: that the flag set DESIGN
# section 6.5 specifies is accepted by a real cmake, that D22's
# `$ORIGIN/../lib` install RPATH really does make the installed tree
# relocatable, and that the three binaries section 6.5's `install` phase asserts
# land where the layout says.
#
# It deliberately does NOT declare the LLAMA_* options the configure line
# passes: cmake reports unused -D variables as a warning, never an error, so
# this project sees exactly the argv llama.cpp would and still builds in a
# second.
cmake_minimum_required(VERSION 3.14)
project(tinyllama C)

# BUILD_SHARED_LIBS=ON is passed by the configure line; the library below is
# deliberately explicit about being SHARED so the RPATH assertion means
# something either way.
add_library(ggml-base SHARED src/ggml.c)
set_target_properties(ggml-base PROPERTIES SOVERSION 0)

add_executable(llama-server src/server.c)
target_link_libraries(llama-server PRIVATE ggml-base)

add_executable(llama-bench src/bench.c)
target_link_libraries(llama-bench PRIVATE ggml-base)

add_executable(llama-cli src/cli.c)
target_link_libraries(llama-cli PRIVATE ggml-base)

install(TARGETS llama-server llama-bench llama-cli
        RUNTIME DESTINATION bin)
install(TARGETS ggml-base
        LIBRARY DESTINATION lib)
