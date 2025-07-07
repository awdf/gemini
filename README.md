# Go AI Voice/Text Assistant

This project is a command-line AI assistant written in Go. It can accept commands via both voice and text, process them using the Google Gemini API, and display the results in the terminal. It features a decoupled, event-driven architecture designed for clarity and extensibility.

## Features

- **Dual Input Modes**: Accepts commands via both voice and direct text entry.
- **Voice Activity Detection (VAD)**: Listens for audio and only records when speech is detected, saving resources and improving accuracy.
- **Real-time Audio Visualization**: Displays a live RMS soundbar in the terminal so the user knows when the microphone is active.
- **Decoupled Architecture**: Components communicate via Go channels and a central Event Bus, minimizing direct dependencies and making the system easier to maintain and test.
- **AI Integration**: Leverages the Gemini API for powerful natural language processing and response generation.
- **Graceful Shutdown**: Handles `Ctrl+C` to ensure all components shut down cleanly.

---

## Architecture

The application is broken down into several distinct components, each with a specific responsibility. The `App` component acts as an orchestrator and dependency injector, initializing all services and communication channels at startup.

Communication is handled in two primary ways:
1.  **Go Channels**: For high-throughput, point-to-point data streams (e.g., audio data from the `Pipeline` to `VAD`).
2.  **Event Bus**: For decoupled, many-to-many notifications (e.g., `VAD` publishing a "start recording" event that the `Recorder` subscribes to).

### Component Diagram

This diagram shows the static components of the system and their relationships.

!Component Diagram

---

## Interaction Flow

The sequence diagram below illustrates the two primary user interaction flows: a voice command and a text command. It shows how the components collaborate over time to process a user's request.

### Sequence Diagram

!Sequence Diagram

---

## Getting Started

### Prerequisites

- **Go**: Version 1.21 or later.
- **GStreamer**: The core audio processing pipeline relies on GStreamer. You must have the GStreamer core library and base plugins installed on your system.
- **Gemini API Key**: You need an API key from Google AI Studio.

### Installation & Setup

1.  **Clone the repository:**
    ```sh
    git clone <your-repo-url>
    cd <your-repo-directory>
    ```

2.  **Configure the application:**
    The application is configured via a `config.json` file or environment variables. Create a `config.json` in the root directory:
    ```json
    {
      "api_key": "YOUR_GEMINI_API_KEY"
    }
    ```
    Alternatively, set the environment variable:
    ```sh
    export GEMINI_API_KEY="YOUR_GEMINI_API_KEY"
    ```

3.  **Build the application:**
    ```sh
    go build -o assistant .
    ```

### Running the Application

Execute the compiled binary from your terminal:

```sh
./assistant
```

- To issue a **voice command**, simply speak when the application is running. The RMS soundbar will indicate that it is listening.
- To issue a **text command**, type your prompt and press `Enter`.
- To **exit**, press `Ctrl+C`.

---

## Diagrams

The architecture diagrams are located in the `/UML` directory and are written using PlantUML. If you make changes to the `.puml` files, you can regenerate the PNG images for this README.

A simple way to do this is with the official PlantUML JAR:

```sh
java -jar plantuml.jar UML/*.puml
```

This will generate `components.puml.png` and `sequence.puml.png` in the `UML/` directory.