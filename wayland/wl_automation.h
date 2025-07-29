#ifndef YOUR_LIB_H_WL_AUTOMATION
#define YOUR_LIB_H_WL_AUTOMATION

#include <stdbool.h>
#include <linux/joystick.h>
#include <stdint.h>

#define BTN_REPEATED    2
#define BTN_DOWN        1
#define BTN_UP          0

struct frame {
    unsigned int width;
    unsigned int height;
    unsigned int stride;
    unsigned int size;
    void* data;
};

struct key {
    int type;
    int code;
    int value;
};

enum device_type {
    DEVICE_KEYBOARD,
    DEVICE_MOUSE,
    DEVICE_JOYSTICK
};

bool init();
bool initManualy(int xmax, int ymax, int bpp);
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

int kbdRead(struct key *key);
int kbdWrite(struct key key);

void disableJoystick();
struct js_event* readJoystickEvent();
void freeJoystickEvent(struct js_event* event);
int vibrateJoystick(uint16_t lowFrequencyMotor, uint16_t highFrequencyMotor, uint32_t durationMs);

void type(const char *str);
void keyAction(const int key_codes[], int count, int state);

bool isDeviceAvailable(enum device_type type);

#endif //YOUR_LIB_H_WL_AUTOMATION
