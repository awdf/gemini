#ifndef YOUR_LIB_H_WL_AUTOMATION
#define YOUR_LIB_H_WL_AUTOMATION

#include <stdbool.h>
#include <stdint.h>

struct js_event {
	unsigned int time;     // event timestamp in milliseconds
	short value;           // value
	unsigned char etype;    // event type
	unsigned char number;  // axis/button number
};

struct frame {
    unsigned int width;
    unsigned int height;
    unsigned int stride;
    unsigned int size;
    void* data;
};

bool init();
void done();

struct frame* mapScreen();
void unmapScreen(struct frame *frame);

void setAccuracy(int value);
void setOffset(int value);
void mouseMoveRel(int dx, int dy);
void mouseMoveAbs(int x, int y);
void mouseLeft(int state);
void mouseStep(int x, int y);
void mousePos(int *x, int *y);

void touchpadTap(int x, int y, int pressure);
void touchpadRelease();

int kbd_read();

void disableJoystick();
struct js_event* readJoystickEvent();
void freeJoystickEvent(struct js_event* event);
int vibrateJoystick(uint16_t lowFrequencyMotor, uint16_t highFrequencyMotor, uint32_t durationMs);

#endif //YOUR_LIB_H_WL_AUTOMATION