/* The shared library the three binaries link against.
 *
 * Its only job is to be somewhere OTHER than beside the executables, so that a
 * binary which runs after being installed proves the $ORIGIN/../lib RPATH (D22)
 * rather than proving that the loader found a library in the same directory. */
int tiny_build_number(void) { return 6000; }
