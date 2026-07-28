#include <stdio.h>
#include <stdlib.h>

/* Compiled at -O2: locals get optimized out and line numbers jump around. */
static long accumulate(long n)
{
	long acc = 0;
	long i;

	for (i = 1; i <= n; i++)
		acc += i * i;
	return acc;
}

int main(int argc, char **argv)
{
	long n = argc > 1 ? strtol(argv[1], NULL, 10) : 100;
	long result = accumulate(n);

	printf("result=%ld\n", result);
	return 0;
}
