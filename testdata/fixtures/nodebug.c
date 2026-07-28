#include <stdio.h>

/* Built without -g and stripped: disassembly is the only available view. */
static int mangle(int x)
{
	return (x << 3) ^ (x >> 1);
}

int main(void)
{
	int i;
	int acc = 0;

	for (i = 0; i < 5; i++)
		acc += mangle(i);
	printf("acc=%d\n", acc);
	return 0;
}
