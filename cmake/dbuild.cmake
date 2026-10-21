# Inject with -DCMAKE_PROJECT_INCLUDE=<path to this file> (no project changes needed).
#
# Projects often cap Ninja compile parallelism with a job pool sized for one machine
# (CMAKE_JOB_POOL_COMPILE). With dbuild the daemon does the throttling, so after the
# top-level CMakeLists finishes, raise that pool to DBUILD_POOL_DEPTH (default 256).
include_guard(GLOBAL)

if(NOT DEFINED DBUILD_POOL_DEPTH)
    set(DBUILD_POOL_DEPTH 256)
endif()

function(_dbuild_raise_compile_pool)
    if(NOT CMAKE_JOB_POOL_COMPILE)
        return()
    endif()
    get_property(pools GLOBAL PROPERTY JOB_POOLS)
    set(out)
    foreach(p IN LISTS pools)
        string(REGEX REPLACE "=.*" "" name "${p}")
        if(name STREQUAL CMAKE_JOB_POOL_COMPILE)
            list(APPEND out "${name}=${DBUILD_POOL_DEPTH}")
        else()
            list(APPEND out "${p}")
        endif()
    endforeach()
    set_property(GLOBAL PROPERTY JOB_POOLS ${out})
endfunction()

cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL _dbuild_raise_compile_pool)
